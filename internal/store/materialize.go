package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/teamlead-com/cerbix/internal/dispatch"
	"github.com/teamlead-com/cerbix/internal/domain"
	"github.com/teamlead-com/cerbix/internal/secret"
)

const (
	MaterializeSkippedCurrentState = "skipped_current_state"
	MaterializeMissingReference    = "missing_reference"
	MaterializeDecryptFailed       = "decrypt_failed"
	MaterializeNoDispatchKey       = "no_dispatch_key"
	MaterializePayloadTooLarge     = "payload_too_large"
	maxMaterializedJobBytes        = 1 << 20
	materializeQueryTimeout        = 10 * time.Second
)

// MaterializedExecution is one per-monitor result from a bounded authoritative batch.
// A non-empty Reason means no job may be dispatched and cadence must not advance as sent.
type MaterializedExecution struct {
	MonitorID string
	Job       dispatch.CheckJob
	Reason    string
}

type materializedRef struct {
	SecretID   string `json:"secret_id"`
	Name       string `json:"name"`
	Ciphertext string `json:"ciphertext"`
}

// MaterializeExecutionConfigs is the authoritative dispatch-authorization read (§4.4.3).
// It reads every execution field plus tenant-safe refs/ciphertexts in ONE statement, then
// immediately decrypts and re-wraps credentials. Plaintext never enters a returned DTO,
// scheduler snapshot, cache, database queue, or broker payload.
func (s *Store) MaterializeExecutionConfigs(ctx context.Context, monitorIDs []string) ([]MaterializedExecution, error) {
	if len(monitorIDs) == 0 {
		return nil, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, materializeQueryTimeout)
	defer cancel()
	rows, err := s.pool.Query(queryCtx,
		`SELECT `+monitorColumns+`, m.config, gen_random_uuid()::text,
		        COALESCE(refs.bound, '{}'::jsonb)
		   FROM monitors m
		   LEFT JOIN LATERAL (
		     SELECT jsonb_object_agg(r.setting_key,
		              jsonb_build_object('secret_id', ps.id::text, 'name', ps.name,
		                                 'ciphertext', ps.value_encrypted)) AS bound
		       FROM monitor_secret_refs r
		       JOIN project_secrets ps
		         ON ps.id = r.secret_id
		        AND ps.project_id = r.project_id
		        AND ps.project_id = m.project_id
		      WHERE r.monitor_id = m.id
		        AND r.project_id = m.project_id
		   ) refs ON true
		  WHERE m.id = ANY($1::uuid[])
		  ORDER BY m.id`, monitorIDs)
	if err != nil {
		return nil, fmt.Errorf("store: materialize execution batch: %w", err)
	}
	defer rows.Close()

	byID := make(map[string]MaterializedExecution, len(monitorIDs))
	for rows.Next() {
		var rawConfig, rawRefs []byte
		var jobID string
		m, err := s.scanMonitorNoSecrets(rows, &rawConfig, &jobID, &rawRefs)
		if err != nil {
			return nil, fmt.Errorf("store: scan materialized execution: %w", err)
		}
		entry := MaterializedExecution{MonitorID: m.ID}
		if !m.Enabled || !m.Type.Active() {
			entry.Reason = MaterializeSkippedCurrentState
			byID[m.ID] = entry
			continue
		}
		job := dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV1}
		if !domain.CredentialedType(m.Type) {
			entry.Job = job
			byID[m.ID] = entry
			continue
		}
		stored := map[string]string{}
		if err := json.Unmarshal(rawConfig, &stored); err != nil {
			entry.Reason = MaterializeDecryptFailed
			byID[m.ID] = entry
			continue
		}
		bound := map[string]materializedRef{}
		if err := json.Unmarshal(rawRefs, &bound); err != nil {
			entry.Reason = MaterializeMissingReference
			byID[m.ID] = entry
			continue
		}
		fields := map[string][]byte{}
		if refName := stored["password_ref"]; refName != "" {
			ref, ok := bound["password_ref"]
			if !ok || ref.Name != refName || ref.SecretID == "" || ref.Ciphertext == "" || s.cipher == nil {
				entry.Reason = MaterializeMissingReference
				byID[m.ID] = entry
				continue
			}
			plain, err := s.cipher.DecryptBytes(ref.Ciphertext, secret.CanonicalAAD(m.ProjectID, ref.SecretID))
			if err != nil {
				entry.Reason = MaterializeDecryptFailed
				byID[m.ID] = entry
				continue
			}
			fields["password"] = plain
		} else if encrypted := stored["password"]; encrypted != "" {
			if s.cipher == nil {
				entry.Reason = MaterializeDecryptFailed
				byID[m.ID] = entry
				continue
			}
			plain, err := s.cipher.Decrypt(encrypted)
			if err != nil {
				entry.Reason = MaterializeDecryptFailed
				byID[m.ID] = entry
				continue
			}
			fields["password"] = []byte(plain)
		}
		if len(fields) > 0 {
			ring, ok := s.credentialKeyrings.ForRegion(m.Region)
			if !ok {
				dispatch.WipeCredentialFields(fields)
				entry.Reason = MaterializeNoDispatchKey
				byID[m.ID] = entry
				continue
			}
			// Generation 1 for now: the emitter moves to generation 2 with the carrier-3
			// rollout, and until then a v2 envelope would reach executors that cannot open it.
			envelope, err := ring.Seal(dispatch.SealContext{
				EnvelopeVersion: dispatch.EnvelopeV1,
				Region:          m.Region,
				JobID:           jobID,
				MonitorID:       m.ID,
				Revision:        m.ExecutionRevision,
				Body:            job.Monitor,
			}, fields)
			dispatch.WipeCredentialFields(fields)
			if err != nil {
				entry.Reason = MaterializeDecryptFailed
				byID[m.ID] = entry
				continue
			}
			job.ProtocolVersion = dispatch.ProtocolV2
			job.CredentialEnvelope = envelope
			body, err := json.Marshal(job)
			if err != nil || len(body) > maxMaterializedJobBytes {
				entry.Reason = MaterializePayloadTooLarge
				byID[m.ID] = entry
				continue
			}
		}
		entry.Job = job
		byID[m.ID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate materialized executions: %w", err)
	}

	// Preserve caller order and represent deleted/missing candidates as current-state skips.
	out := make([]MaterializedExecution, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		if entry, ok := byID[id]; ok {
			out = append(out, entry)
		} else {
			out = append(out, MaterializedExecution{MonitorID: id, Reason: MaterializeSkippedCurrentState})
		}
	}
	return out, nil
}

// MaterializeExecutionConfig is the singular test/manual convenience over the same
// authoritative batch implementation.
func (s *Store) MaterializeExecutionConfig(ctx context.Context, monitorID string) (MaterializedExecution, error) {
	items, err := s.MaterializeExecutionConfigs(ctx, []string{monitorID})
	if err != nil {
		return MaterializedExecution{}, err
	}
	return items[0], nil
}

// MaterializeTestExecutionConfig seals an unsaved create-form test. The submitted monitor
// is the authoritative config; inventory resolution, tenant binding and both generated ids
// happen in one DB statement. A raw password is accepted only on this API surface and is
// immediately wrapped — never persisted or handed to a transport in plaintext.
func (s *Store) MaterializeTestExecutionConfig(ctx context.Context, m domain.Monitor) (MaterializedExecution, error) {
	if !domain.CredentialedType(m.Type) {
		return MaterializedExecution{Job: dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV1}}, nil
	}
	var projectID, monitorID, jobID string
	fields := map[string][]byte{}
	if refName := m.Config["password_ref"]; refName != "" {
		var secretID, ciphertext string
		err := s.pool.QueryRow(ctx,
			`SELECT p.id::text, ps.id::text, ps.value_encrypted,
			        gen_random_uuid()::text, gen_random_uuid()::text
			   FROM projects p
			   JOIN project_secrets ps ON ps.project_id=p.id AND ps.name=$2
			  WHERE p.id=$1`, m.ProjectID, refName).
			Scan(&projectID, &secretID, &ciphertext, &monitorID, &jobID)
		if noRows(err) {
			return MaterializedExecution{Reason: MaterializeMissingReference}, nil
		}
		if err != nil {
			return MaterializedExecution{}, fmt.Errorf("store: materialize test ref: %w", err)
		}
		if s.cipher == nil {
			return MaterializedExecution{Reason: MaterializeDecryptFailed}, nil
		}
		plain, err := s.cipher.DecryptBytes(ciphertext, secret.CanonicalAAD(projectID, secretID))
		if err != nil {
			return MaterializedExecution{Reason: MaterializeDecryptFailed}, nil
		}
		fields["password"] = plain
	} else {
		if err := s.pool.QueryRow(ctx,
			`SELECT id::text, gen_random_uuid()::text, gen_random_uuid()::text
			   FROM projects WHERE id=$1`, m.ProjectID).Scan(&projectID, &monitorID, &jobID); noRows(err) {
			return MaterializedExecution{Reason: MaterializeSkippedCurrentState}, nil
		} else if err != nil {
			return MaterializedExecution{}, fmt.Errorf("store: materialize inline test: %w", err)
		}
		if password, ok := m.Config["password"]; ok {
			fields["password"] = []byte(password)
		}
	}
	m.ProjectID = projectID
	m.ID = monitorID
	m.ExecutionRevision = 1
	configCopy := make(map[string]string, len(m.Config))
	for key, value := range m.Config {
		configCopy[key] = value
	}
	m.Config = configCopy
	delete(m.Config, "password")
	job := dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV1}
	if len(fields) == 0 {
		return MaterializedExecution{MonitorID: monitorID, Job: job}, nil
	}
	ring, ok := s.credentialKeyrings.ForRegion(m.Region)
	if !ok {
		dispatch.WipeCredentialFields(fields)
		return MaterializedExecution{MonitorID: monitorID, Reason: MaterializeNoDispatchKey}, nil
	}
	envelope, err := ring.Seal(dispatch.SealContext{
		EnvelopeVersion: dispatch.EnvelopeV1,
		Region:          m.Region,
		JobID:           jobID,
		MonitorID:       monitorID,
		Revision:        1,
		Body:            job.Monitor,
	}, fields)
	dispatch.WipeCredentialFields(fields)
	if err != nil {
		return MaterializedExecution{MonitorID: monitorID, Reason: MaterializeDecryptFailed}, nil
	}
	job.ProtocolVersion = dispatch.ProtocolV2
	job.CredentialEnvelope = envelope
	return MaterializedExecution{MonitorID: monitorID, Job: job}, nil
}
