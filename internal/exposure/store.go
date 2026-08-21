package exposure

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Migrations returns the schema migrations owned by the exposure controller.
func (s *Service) Migrations() []store.Migration {
	return []store.Migration{
		{
			Name: "0001_exposure_services_and_acks",
			SQL: `
CREATE TABLE exposure_services (
    id            TEXT PRIMARY KEY,
    hostname      TEXT NOT NULL UNIQUE,
    home_anchor   TEXT NOT NULL,
    upstream      TEXT NOT NULL,
    tunnel_origin TEXT NOT NULL,
    exposure      TEXT NOT NULL,
    app_auth      TEXT NOT NULL,
    revision      INTEGER NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE TABLE exposure_public_acks (
    service_id TEXT PRIMARY KEY,
    ack_id     TEXT NOT NULL,
    revision   INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
`,
		},
		{
			Name: "0002_exposure_plans",
			SQL: `
CREATE TABLE exposure_plans (
    id               TEXT PRIMARY KEY,
    service_id       TEXT NOT NULL,
    service_revision INTEGER NOT NULL,
    kind             TEXT NOT NULL,
    hostname         TEXT NOT NULL,
    from_exposure    TEXT NOT NULL,
    to_exposure      TEXT NOT NULL,
    steps            TEXT NOT NULL,
    warnings         TEXT NOT NULL,
    requires_ack     INTEGER NOT NULL,
    status           TEXT NOT NULL,
    results          TEXT NOT NULL,
    last_error       TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    applied_at       TEXT
);
CREATE INDEX exposure_plans_by_service ON exposure_plans (service_id, created_at);
`,
		},
		{
			Name: "0003_exposure_observations",
			SQL: `
CREATE TABLE exposure_observations (
    service_id     TEXT PRIMARY KEY,
    observed_at    TEXT NOT NULL,
    vanity_dns     TEXT NOT NULL,
    anchor_dns     TEXT NOT NULL,
    tunnel_ingress TEXT NOT NULL,
    access_app     TEXT NOT NULL,
    edge_route     TEXT NOT NULL,
    reconciled     INTEGER NOT NULL,
    drift          TEXT NOT NULL,
    last_error     TEXT NOT NULL,
    plan_id        TEXT NOT NULL
);
`,
		},
	}
}

const serviceColumns = `id, hostname, home_anchor, upstream, tunnel_origin, exposure, app_auth, revision, created_at, updated_at`

func (s *Service) insertService(ctx context.Context, rec *ServiceRecord) error {
	_, err := s.db.DB().ExecContext(ctx, `INSERT INTO exposure_services (`+serviceColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Hostname, rec.HomeAnchor, rec.Upstream, rec.TunnelOrigin,
		string(rec.Exposure), string(rec.AppAuth), rec.Revision,
		store.FormatTime(rec.CreatedAt), store.FormatTime(rec.UpdatedAt))
	return err
}

func (s *Service) updateService(ctx context.Context, rec *ServiceRecord) error {
	_, err := s.db.DB().ExecContext(ctx,
		`UPDATE exposure_services SET upstream = ?, tunnel_origin = ?, exposure = ?, app_auth = ?, revision = ?, updated_at = ? WHERE id = ?`,
		rec.Upstream, rec.TunnelOrigin, string(rec.Exposure), string(rec.AppAuth), rec.Revision, store.FormatTime(rec.UpdatedAt), rec.ID)
	return err
}

func scanService(row interface{ Scan(dest ...any) error }) (ServiceRecord, error) {
	var rec ServiceRecord
	var exposure, appAuth, created, updated string
	if err := row.Scan(&rec.ID, &rec.Hostname, &rec.HomeAnchor, &rec.Upstream, &rec.TunnelOrigin,
		&exposure, &appAuth, &rec.Revision, &created, &updated); err != nil {
		return ServiceRecord{}, err
	}
	rec.Exposure = domain.Exposure(exposure)
	rec.AppAuth = AuthMode(appAuth)
	rec.CreatedAt = parseStoredTime(created)
	rec.UpdatedAt = parseStoredTime(updated)
	return rec, nil
}

func (s *Service) getService(ctx context.Context, id string) (ServiceRecord, error) {
	rec, err := scanService(s.db.DB().QueryRowContext(ctx, `SELECT `+serviceColumns+` FROM exposure_services WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRecord{}, store.NotFoundf("service %q", id)
	}
	return rec, err
}

func (s *Service) getServiceByHostname(ctx context.Context, hostname string) (ServiceRecord, error) {
	rec, err := scanService(s.db.DB().QueryRowContext(ctx, `SELECT `+serviceColumns+` FROM exposure_services WHERE hostname = ?`, hostname))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRecord{}, store.NotFoundf("service %q", hostname)
	}
	return rec, err
}

func (s *Service) listServices(ctx context.Context) ([]ServiceRecord, error) {
	rows, err := s.db.DB().QueryContext(ctx, `SELECT `+serviceColumns+` FROM exposure_services ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceRecord{}
	for rows.Next() {
		rec, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// deleteServiceRows removes a service and its dependent rows. Plans are kept
// deliberately as the audit trail.
func (s *Service) deleteServiceRows(ctx context.Context, id string) error {
	tx, err := s.db.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM exposure_services WHERE id = ?`,
		`DELETE FROM exposure_public_acks WHERE service_id = ?`,
		`DELETE FROM exposure_observations WHERE service_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// publicAck is a revision-bound acknowledgement for public exposure.
type publicAck struct {
	ServiceID string
	AckID     string
	Revision  int64
	CreatedAt time.Time
}

func (s *Service) putAck(ctx context.Context, ack publicAck) error {
	_, err := s.db.DB().ExecContext(ctx, `
INSERT INTO exposure_public_acks (service_id, ack_id, revision, created_at) VALUES (?,?,?,?)
ON CONFLICT(service_id) DO UPDATE SET
  ack_id = excluded.ack_id,
  revision = excluded.revision,
  created_at = excluded.created_at`,
		ack.ServiceID, ack.AckID, ack.Revision, store.FormatTime(ack.CreatedAt))
	return err
}

func (s *Service) getAck(ctx context.Context, serviceID string) (publicAck, bool, error) {
	var ack publicAck
	var created string
	err := s.db.DB().QueryRowContext(ctx,
		`SELECT service_id, ack_id, revision, created_at FROM exposure_public_acks WHERE service_id = ?`, serviceID).
		Scan(&ack.ServiceID, &ack.AckID, &ack.Revision, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ack, false, nil
	}
	if err != nil {
		return ack, false, err
	}
	ack.CreatedAt = parseStoredTime(created)
	return ack, true, nil
}

const planColumns = `id, service_id, service_revision, kind, hostname, from_exposure, to_exposure, steps, warnings, requires_ack, status, results, last_error, created_at, applied_at`

func (s *Service) insertPlan(ctx context.Context, p *Plan) error {
	_, err := s.db.DB().ExecContext(ctx, `INSERT INTO exposure_plans (`+planColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.ServiceID, p.ServiceRevision, string(p.Kind), p.Hostname,
		string(p.FromExposure), string(p.ToExposure),
		marshalJSON(p.Steps), marshalJSON(p.Warnings), boolInt(p.RequiresAck),
		string(p.Status), marshalJSON(p.Results), p.Error,
		store.FormatTime(p.CreatedAt), timeColumn(p.AppliedAt))
	return err
}

func (s *Service) updatePlan(ctx context.Context, p *Plan) error {
	_, err := s.db.DB().ExecContext(ctx,
		`UPDATE exposure_plans SET status = ?, results = ?, last_error = ?, applied_at = ? WHERE id = ?`,
		string(p.Status), marshalJSON(p.Results), p.Error, timeColumn(p.AppliedAt), p.ID)
	return err
}

func scanPlan(row interface{ Scan(dest ...any) error }) (Plan, error) {
	var p Plan
	var kind, from, to, status, lastErr, created string
	var stepsJSON, warningsJSON, resultsJSON string
	var requiresAck int
	var applied sql.NullString
	if err := row.Scan(&p.ID, &p.ServiceID, &p.ServiceRevision, &kind, &p.Hostname,
		&from, &to, &stepsJSON, &warningsJSON, &requiresAck, &status, &resultsJSON, &lastErr, &created, &applied); err != nil {
		return Plan{}, err
	}
	p.Kind = PlanKind(kind)
	p.FromExposure = domain.Exposure(from)
	p.ToExposure = domain.Exposure(to)
	p.RequiresAck = requiresAck != 0
	p.Status = PlanStatus(status)
	p.Error = lastErr
	p.CreatedAt = parseStoredTime(created)
	if applied.Valid {
		t := parseStoredTime(applied.String)
		p.AppliedAt = &t
	}
	if err := json.Unmarshal([]byte(stepsJSON), &p.Steps); err != nil {
		return Plan{}, fmt.Errorf("decoding steps of plan %s: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(warningsJSON), &p.Warnings); err != nil {
		return Plan{}, fmt.Errorf("decoding warnings of plan %s: %w", p.ID, err)
	}
	if resultsJSON != "" {
		if err := json.Unmarshal([]byte(resultsJSON), &p.Results); err != nil {
			return Plan{}, fmt.Errorf("decoding results of plan %s: %w", p.ID, err)
		}
	}
	return p, nil
}

func (s *Service) getPlan(ctx context.Context, id string) (Plan, error) {
	p, err := scanPlan(s.db.DB().QueryRowContext(ctx, `SELECT `+planColumns+` FROM exposure_plans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, store.NotFoundf("plan %q", id)
	}
	return p, err
}

func (s *Service) listPlans(ctx context.Context, serviceID string, limit int) ([]Plan, error) {
	rows, err := s.db.DB().QueryContext(ctx,
		`SELECT `+planColumns+` FROM exposure_plans WHERE service_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) putObservation(ctx context.Context, obs *Observation, planID string) error {
	drift := obs.Drift
	if drift == nil {
		drift = []string{}
	}
	_, err := s.db.DB().ExecContext(ctx, `
INSERT INTO exposure_observations
  (service_id, observed_at, vanity_dns, anchor_dns, tunnel_ingress, access_app, edge_route, reconciled, drift, last_error, plan_id)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(service_id) DO UPDATE SET
  observed_at = excluded.observed_at,
  vanity_dns = excluded.vanity_dns,
  anchor_dns = excluded.anchor_dns,
  tunnel_ingress = excluded.tunnel_ingress,
  access_app = excluded.access_app,
  edge_route = excluded.edge_route,
  reconciled = excluded.reconciled,
  drift = excluded.drift,
  last_error = excluded.last_error,
  plan_id = excluded.plan_id`,
		obs.ServiceID, store.FormatTime(obs.ObservedAt),
		marshalOpt(obs.VanityDNS), marshalOpt(obs.AnchorDNS),
		marshalOpt(obs.TunnelIngress), marshalOpt(obs.AccessApp), marshalOpt(obs.EdgeRoute),
		boolInt(obs.Reconciled), marshalJSON(drift), obs.Error, planID)
	return err
}

func (s *Service) getObservation(ctx context.Context, serviceID string) (Observation, bool, error) {
	var o Observation
	var observedAt, vanity, anchor, ingress, app, route, drift, lastErr, planID string
	var reconciled int
	err := s.db.DB().QueryRowContext(ctx, `
SELECT observed_at, vanity_dns, anchor_dns, tunnel_ingress, access_app, edge_route, reconciled, drift, last_error, plan_id
FROM exposure_observations WHERE service_id = ?`, serviceID).
		Scan(&observedAt, &vanity, &anchor, &ingress, &app, &route, &reconciled, &drift, &lastErr, &planID)
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	o.ServiceID = serviceID
	o.ObservedAt = parseStoredTime(observedAt)
	o.Reconciled = reconciled != 0
	o.Error = lastErr
	o.PlanID = planID
	if v, err := unmarshalOpt[Record](vanity); err != nil {
		return o, true, fmt.Errorf("decoding vanity observation: %w", err)
	} else {
		o.VanityDNS = v
	}
	if v, err := unmarshalOpt[Record](anchor); err != nil {
		return o, true, fmt.Errorf("decoding anchor observation: %w", err)
	} else {
		o.AnchorDNS = v
	}
	if v, err := unmarshalOpt[IngressRule](ingress); err != nil {
		return o, true, fmt.Errorf("decoding ingress observation: %w", err)
	} else {
		o.TunnelIngress = v
	}
	if v, err := unmarshalOpt[AccessApp](app); err != nil {
		return o, true, fmt.Errorf("decoding access observation: %w", err)
	} else {
		o.AccessApp = v
	}
	if v, err := unmarshalOpt[Route](route); err != nil {
		return o, true, fmt.Errorf("decoding edge observation: %w", err)
	} else {
		o.EdgeRoute = v
	}
	if drift != "" {
		if err := json.Unmarshal([]byte(drift), &o.Drift); err != nil {
			return o, true, fmt.Errorf("decoding drift: %w", err)
		}
	}
	return o, true, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func timeColumn(t *time.Time) any {
	if t == nil {
		return nil
	}
	return store.FormatTime(*t)
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// The persisted types marshal by construction.
		panic("exposure: marshal: " + err.Error())
	}
	return string(b)
}

func marshalOpt[T any](v *T) string {
	if v == nil {
		return ""
	}
	return marshalJSON(v)
}

func unmarshalOpt[T any](s string) (*T, error) {
	if s == "" {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return &v, nil
}
