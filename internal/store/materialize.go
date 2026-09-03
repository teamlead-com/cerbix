package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// MaterializeCarrierTooOld: a scenario secret binding needs an envelope that binds the
	// execution body, which only the newest carrier generation issues. A region still on an
	// older carrier gets this per-monitor reason instead of a job whose credential could be
	// replayed into a relocated placeholder (FR-028 D8a).
	MaterializeCarrierTooOld = "carrier_too_old"
	maxMaterializedJobBytes  = 1 << 20
	materializeQueryTimeout  = 10 * time.Second
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
// envelopeForCarrier maps a carrier generation to the envelope generation it carries. The
// mapping is EXACT: an unknown carrier is an error, not a guess. The earlier version
// promised to "degrade to the oldest envelope" and then returned the NEWEST for anything
// above 3, leaving the job's protocol version at the unknown value — the comment and the
// code disagreed, and the honest answer is that a carrier we do not know is a wiring bug we
// must not paper over on either side.
func envelopeForCarrier(carrierGeneration int) (int, error) {
	switch carrierGeneration {
	case dispatch.ProtocolV1, dispatch.ProtocolV2:
		return dispatch.EnvelopeV1, nil
	case dispatch.ProtocolV3:
		return dispatch.EnvelopeV2, nil
	default:
		return 0, fmt.Errorf("store: no envelope generation for carrier %d", carrierGeneration)
	}
}

// MaterializeExecutionConfigs builds dispatch-ready jobs for the given monitors.
//
// carrierByRegion is the caller's per-region policy: the carrier it has ESTABLISHED that
// region can consume. It is a MAP and not a scalar because the region a job belongs to is
// only known from the authoritative read — the snapshot's region may be stale. Choosing the
// carrier from the snapshot meant a monitor moved from a capability-2 region to a
// capability-1 one just before the read got the right row and the right key but the WRONG
// carrier, and its job then sat on a queue nobody in the new region consumes (§4.4.3:
// regroup by the authoritative region, and only then select the transport).
//
// A region absent from the policy falls back to the generation every capable executor
// understands.
func (s *Store) MaterializeExecutionConfigs(ctx context.Context, monitorIDs []string, carrierByRegion map[string]int) ([]MaterializedExecution, error) {
	if len(monitorIDs) == 0 {
		return nil, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, materializeQueryTimeout)
	defer cancel()
	rows, err := s.pool.Query(queryCtx,
		`SELECT `+monitorColumns+`, m.config, gen_random_uuid()::text, statement_timestamp(),
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
		var issuedAt time.Time
		// FR-028: the job needs the SCENARIO decrypted (it is execution input), and must
		// still never receive the credential in config — that arrives as an envelope. The
		// safe reader used here before stage 1 omitted nothing, because nothing in config was
		// encrypted except the credential; once the scenario joined the encrypted set, this
		// line silently stripped it and every synthetic probe would have run with no scenario.
		m, err := s.scanMonitorForExecution(rows, &rawConfig, &jobID, &issuedAt, &rawRefs)
		if err != nil {
			return nil, fmt.Errorf("store: scan materialized execution: %w", err)
		}
		entry := MaterializedExecution{MonitorID: m.ID}
		if !m.Enabled || !m.Type.Active() {
			entry.Reason = MaterializeSkippedCurrentState
			byID[m.ID] = entry
			continue
		}
		job := dispatch.CheckJob{Monitor: m, ProtocolVersion: dispatch.ProtocolV1, JobID: jobID, IssuedAt: issuedAt}
		stored := map[string]string{}
		if err := json.Unmarshal(rawConfig, &stored); err != nil {
			entry.Reason = MaterializeDecryptFailed
			byID[m.ID] = entry
			continue
		}
		// Synthetic only, and precisely: this gate stops a `scenario_secret_*` key on another
		// type from pulling that monitor onto the credential path, so the monitor KEEPS
		// PROBING instead of failing for a key it cannot use. It does NOT make such a key
		// harmless everywhere — an existing `monitor_secret_refs` row still counts against
		// deleting its secret and still follows a rename, and the stored key blocks the
		// monitor's next API edit until it is dropped. No released build ever accepted the
		// key; `TestAPreFixNonSyntheticBindingRowBehavesAsDocumented` seeds one anyway and
		// states each of those outcomes, because the review was right that "inert" was not
		// what this gate delivers.
		// FR-029 D8: the RUN key, set before the job is sealed so the execution digest covers it.
		// It is the scheduled WINDOW — floor(now / interval) — and not the job id, so a redelivered
		// AMQP message, a re-claimed pull job after a lease expiry and a transport retry all carry
		// the same idempotency key while the next scheduled run carries a different one. A job that
		// straddles a window boundary on retry gets the next window's key, which is the honest
		// reading: it IS the next run.
		if m.Type == domain.MonitorAsyncCanary && m.IntervalSeconds > 0 {
			stored[domain.CanaryRunKey] = strconv.FormatInt(time.Now().Unix()/int64(m.IntervalSeconds), 10)
			m.Config = stored
			job.Monitor = m
		}
		var scenarioRefKeys []string
		if m.Type == domain.MonitorSynthetic {
			scenarioRefKeys = domain.ScenarioSecretRefKeys(stored)
		}
		// FR-029: a canary's bindings ride the same path — one envelope field per binding, and the
		// same body-bound carrier floor, because the document that says WHERE a credential may be
		// sent is what the digest has to cover.
		var canaryRefKeys []string
		if m.Type == domain.MonitorAsyncCanary {
			canaryRefKeys = domain.CanarySecretRefKeys(stored)
		}
		// A monitor with neither a credential schema nor a scenario binding carries no
		// envelope at all, exactly as before (FR-028 stage 2 adds the second half of this
		// condition and nothing else to the ordinary path).
		if !domain.CredentialedType(m.Type) && len(scenarioRefKeys) == 0 && len(canaryRefKeys) == 0 {
			entry.Job = job
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
		// One envelope field per scenario binding, named for the binding. The executor
		// substitutes them into the scenario after the structural gate and wipes them; they
		// never touch the monitor's config on the way out of here.
		scenarioFailure := ""
		// A binding cannot ride a carrier whose envelope does not bind the body: the
		// anti-relocation property depends on it, so a region still on an older carrier gets
		// a per-monitor reason rather than a job that looks protected and is not.
		if len(scenarioRefKeys)+len(canaryRefKeys) > 0 {
			generation := dispatch.ProtocolV2
			if g, ok := carrierByRegion[m.Region]; ok && g > 0 {
				generation = g
			}
			version, verr := envelopeForCarrier(generation)
			if verr != nil || version < dispatch.EnvelopeV2 {
				entry.Reason = MaterializeCarrierTooOld
				byID[m.ID] = entry
				continue
			}
		}
		for _, key := range canaryRefKeys {
			binding, _ := domain.CanaryBindingFromRefKey(key)
			refName := strings.TrimSpace(stored[key])
			ref, ok := bound[key]
			if !ok || ref.Name != refName || ref.SecretID == "" || ref.Ciphertext == "" || s.cipher == nil {
				scenarioFailure = MaterializeMissingReference
				break
			}
			plain, err := s.cipher.DecryptBytes(ref.Ciphertext, secret.CanonicalAAD(m.ProjectID, ref.SecretID))
			if err != nil {
				scenarioFailure = MaterializeDecryptFailed
				break
			}
			fields[domain.CanaryBindingField(binding)] = plain
		}
		for _, key := range scenarioRefKeys {
			binding, _ := domain.ScenarioBindingFromRefKey(key)
			refName := strings.TrimSpace(stored[key])
			ref, ok := bound[key]
			if !ok || ref.Name != refName || ref.SecretID == "" || ref.Ciphertext == "" || s.cipher == nil {
				scenarioFailure = MaterializeMissingReference
				break
			}
			plain, err := s.cipher.DecryptBytes(ref.Ciphertext, secret.CanonicalAAD(m.ProjectID, ref.SecretID))
			if err != nil {
				scenarioFailure = MaterializeDecryptFailed
				break
			}
			fields[domain.ScenarioBindingField(binding)] = plain
		}
		if scenarioFailure != "" {
			dispatch.WipeCredentialFields(fields)
			entry.Reason = scenarioFailure
			byID[m.ID] = entry
			continue
		}
		if refName := stored["password_ref"]; refName != "" && domain.CredentialedType(m.Type) {
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
		} else if encrypted := stored["password"]; encrypted != "" && domain.CredentialedType(m.Type) {
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
			// The AUTHORITATIVE region decides the carrier, exactly as it already decides
			// the keyring — the two must come from the same row or they can disagree.
			carrierGeneration := dispatch.ProtocolV2
			if g, ok := carrierByRegion[m.Region]; ok && g > 0 {
				carrierGeneration = g
			}
			envelopeVersion, err := envelopeForCarrier(carrierGeneration)
			if err != nil {
				dispatch.WipeCredentialFields(fields)
				entry.Reason = MaterializeDecryptFailed
				byID[m.ID] = entry
				continue
			}
			envelope, err := ring.Seal(dispatch.SealContext{
				EnvelopeVersion: envelopeVersion,
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
			job.ProtocolVersion = carrierGeneration
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
	items, err := s.MaterializeExecutionConfigs(ctx, []string{monitorID}, nil)
	if err != nil {
		return MaterializedExecution{}, err
	}
	return items[0], nil
}

// MaterializeTestExecutionConfig seals an unsaved create-form test. The submitted monitor
// is the authoritative config; inventory resolution, tenant binding and both generated ids
// happen in one DB statement. A raw password is accepted only on this API surface and is
// immediately wrapped — never persisted or handed to a transport in plaintext.
// carrierGeneration is the caller's established carrier for this monitor's region, exactly
// as on the scheduled path: a Test Connection that stayed pinned to generation 2 never
// received the execution binding, so the one-off credential probe kept the target-tamper
// hole the scheduled path had just closed — "jobs AND tests" is a contract, not a slogan.
func (s *Store) MaterializeTestExecutionConfig(ctx context.Context, m domain.Monitor, carrierGeneration int) (MaterializedExecution, error) {
	if carrierGeneration <= 0 {
		carrierGeneration = dispatch.ProtocolV2
	}
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
	envelopeVersion, err := envelopeForCarrier(carrierGeneration)
	if err != nil {
		dispatch.WipeCredentialFields(fields)
		return MaterializedExecution{MonitorID: monitorID, Reason: MaterializeDecryptFailed}, nil
	}
	envelope, err := ring.Seal(dispatch.SealContext{
		EnvelopeVersion: envelopeVersion,
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
	job.ProtocolVersion = carrierGeneration
	job.CredentialEnvelope = envelope
	return MaterializedExecution{MonitorID: monitorID, Job: job}, nil
}
