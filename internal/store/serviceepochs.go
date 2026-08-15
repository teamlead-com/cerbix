package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// Execution-driven evaluation epochs (func-service-reliability §6.2).
//
// A monitor write that changes what the evaluator READS creates an epoch for every service
// whose SLI declares that monitor, in the SAME transaction as the write. A lazy "snapshot
// on next evaluation" was rejected during design: it moves the linearization point into the
// first evaluator run, leaving an interval in which concurrent ingest lands in the wrong
// epoch, and it makes the boundary depend on when a crashed process recovers.
//
// This path deliberately does NOT take the project `service_membership` advisory lock. That
// lock serializes the paths that can change WHICH services a mutation affects — service
// create and delete, declaration writes, monitor delete and move. An ordinary monitor update
// creates epochs, not declarations, so it changes no affected set: a service created
// concurrently writes its own revision-1 epoch, snapshotting this monitor as it then is, so
// nothing is missed. Taking the lock here would serialize every monitor write in a project
// behind one another for no protection at all.

// MaxServicesPerMonitor caps how many epochs one monitor write can create.
//
// This is the HARD maximum of §10.10; the smaller operator-configurable default arrives
// with the settings surface. The cap exists because a routine monitor edit must not take an
// unbounded number of locks, and it is checked while the caller already holds the monitor
// row lock, so two concurrent service writes cannot both pass a stale count.
const MaxServicesPerMonitor = 25

// BumpEpochsForMonitor creates an evaluation epoch for every service whose SLI declares
// this monitor, skipping the ones whose snapshot is unchanged.
//
// It must be called INSIDE the transaction that wrote the monitor, after the monitor row is
// locked and updated: the epoch and the write it describes have to become visible together
// or not at all.
func (s *Store) BumpEpochsForMonitor(ctx context.Context, tx pgx.Tx, projectID, monitorID string) error {
	serviceIDs, err := servicesDeclaringMonitor(ctx, tx, projectID, monitorID)
	if err != nil {
		return err
	}
	if len(serviceIDs) == 0 {
		// A monitor in no service's SLI writes nothing. This is what makes "zero services
		// costs nothing" true rather than rhetorical.
		return nil
	}
	if len(serviceIDs) > MaxServicesPerMonitor {
		return fmt.Errorf("store: monitor %s is a reliability input for %d services, above the %d cap",
			monitorID, len(serviceIDs), MaxServicesPerMonitor)
	}

	// One instant for the whole fan-out, ceiled to a bucket boundary, so every epoch this
	// write creates begins together. Reading the clock per service would let a fan-out
	// straddle a boundary and produce epochs that disagree about when the change happened.
	var effectiveAt time.Time
	if err := tx.QueryRow(ctx,
		`WITH t AS (SELECT statement_timestamp() AS at)
		 SELECT CASE WHEN date_bin('1 minute', at, 'epoch'::timestamptz) = at
		             THEN at
		             ELSE date_bin('1 minute', at, 'epoch'::timestamptz) + interval '1 minute' END
		   FROM t`).Scan(&effectiveAt); err != nil {
		return fmt.Errorf("store: resolve epoch boundary: %w", err)
	}

	for _, serviceID := range serviceIDs {
		if err := s.bumpEpochForService(ctx, tx, projectID, serviceID, effectiveAt); err != nil {
			return err
		}
	}
	return nil
}

// servicesDeclaringMonitor returns, id-ascending, the services whose SLI names this
// monitor. Ordering is the §15.4 lock order, not a convenience: a fan-out that took service
// rows in an arbitrary order would deadlock against any other path holding two of them.
//
// Only `sli` rows count. Operational context decides what is SHOWN on a service; it is not
// an evaluator input, so a change to a diagnostic monitor must not create an epoch.
func servicesDeclaringMonitor(ctx context.Context, tx pgx.Tx, projectID, monitorID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT service_id FROM service_member_refs
		  WHERE monitor_id = $1 AND project_id = $2 AND role = 'sli'
		  ORDER BY service_id`, monitorID, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: services declaring monitor: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan service ref: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) bumpEpochForService(ctx context.Context, tx pgx.Tx, projectID, serviceID string, effectiveAt time.Time) error {
	// Resolve the declaration in force AT THIS EPOCH'S OWN BOUNDARY — including a revision
	// that is pending and not yet in effect.
	//
	// Reading "the revision active right now" instead is the subtle way to break the two
	// axes apart: a service edit at 12:00:10 makes rev2 effective at 12:01, a monitor edit
	// at 12:00:40 lands on the same boundary and displaces rev2's epoch, and the surviving
	// epoch would point at rev1 while 12:01 is governed by rev2. The foreign key would hold
	// and the meaning would not. `effective_at <= $2` is what includes the pending row.
	var revisionID string
	var sli []string
	var policyJSON []byte
	err := tx.QueryRow(ctx,
		`SELECT r.id, r.policies,
		        COALESCE(ARRAY(SELECT m.monitor_id FROM service_definition_members m
		                        WHERE m.revision_id = r.id AND m.role = 'sli'
		                        ORDER BY m.monitor_id), '{}')
		   FROM service_definition_revisions r
		  WHERE r.service_id = $1 AND r.state = 'effective' AND r.effective_at <= $2
		  ORDER BY r.effective_at DESC, r.revision DESC
		  LIMIT 1`, serviceID, effectiveAt).Scan(&revisionID, &policyJSON, &sli)
	if noRows(err) {
		// No declaration governs that boundary yet; there is nothing to snapshot against.
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: resolve declaration at boundary: %w", err)
	}
	if len(sli) == 0 {
		// A service with no reliability inputs produces no facts, so an execution change to
		// a monitor it merely shows has nothing to describe.
		return nil
	}

	var policies domain.ServicePolicies
	if err := json.Unmarshal(policyJSON, &policies); err != nil {
		return fmt.Errorf("store: decode policies: %w", err)
	}

	members, err := s.loadEpochMembers(ctx, tx, projectID, sli, policies)
	if err != nil {
		return err
	}
	hash := domain.EpochSnapshotHash(members)

	// The no-op rule, and this is where it BELONGS: an execution write that leaves the
	// evaluator's inputs byte-identical creates no epoch. A rename bumps execution_revision
	// under the coarse D-0142 fence but changes nothing the evaluator reads, and creating an
	// epoch for it would make the timeline unreadable. The rule applies here and never to a
	// declaration write, where skipping would leave a revision no fact can reference.
	var currentHash string
	err = tx.QueryRow(ctx,
		`SELECT snapshot_hash FROM service_evaluation_epochs
		  WHERE service_id = $1 AND state = 'effective'
		  ORDER BY effective_at DESC, epoch_seq DESC LIMIT 1`, serviceID).Scan(&currentHash)
	if err != nil && !noRows(err) {
		return fmt.Errorf("store: read current epoch hash: %w", err)
	}
	if currentHash == hash {
		return nil
	}

	// Only the epoch axis yields. §10.8 also forbids cancelling durable ranges here: a newer
	// epoch queues its own range and never cancels work another epoch is still finishing.
	if err := supersedeEpochsAtBoundary(ctx, tx, serviceID, effectiveAt); err != nil {
		return err
	}
	snapshot, err := json.Marshal(members)
	if err != nil {
		return fmt.Errorf("store: encode epoch snapshot: %w", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(epoch_seq), 0) + 1 FROM service_evaluation_epochs WHERE service_id = $1`,
		serviceID).Scan(&seq); err != nil {
		return fmt.Errorf("store: next epoch seq: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO service_evaluation_epochs
		   (service_id, project_id, epoch_seq, revision_id, created_at, effective_at, snapshot, snapshot_hash)
		 VALUES ($1,$2,$3,$4, statement_timestamp(), $5,$6,$7)`,
		serviceID, projectID, seq, revisionID, effectiveAt, snapshot, hash); err != nil {
		return fmt.Errorf("store: insert execution epoch: %w", err)
	}
	return nil
}
