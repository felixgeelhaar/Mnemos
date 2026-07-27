package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/mnemos/internal/domain"
	"go.klarlabs.de/mnemos/internal/ports"
)

// LessonRepository persists synthesised lessons in the MySQL backend.
type LessonRepository struct {
	db *sql.DB
}

// Append upserts a lesson. ON DUPLICATE KEY UPDATE refreshes the
// confidence-bearing columns; FK-bearing columns (id) are immutable.
// Snapshots the prior shape into lesson_versions before overwrite.
func (r LessonRepository) Append(ctx context.Context, lesson domain.Lesson) error {
	if err := lesson.Validate(); err != nil {
		return fmt.Errorf("invalid lesson: %w", err)
	}
	if err := r.snapshotIfExists(ctx, lesson.ID); err != nil {
		return fmt.Errorf("snapshot lesson %s: %w", lesson.ID, err)
	}
	source := lesson.Source
	if source == "" {
		source = "synthesize"
	}
	var lastVerified any
	if !lesson.LastVerified.IsZero() {
		lastVerified = lesson.LastVerified.UTC()
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO lessons (id, statement, scope_service, scope_env, scope_team, `+"`trigger`"+`, kind, confidence, derived_at, last_verified, source, created_by, subject_class)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  statement = VALUES(statement),
  confidence = VALUES(confidence),
  derived_at = VALUES(derived_at),
  last_verified = VALUES(last_verified),
  subject_class = VALUES(subject_class)`,
		lesson.ID, lesson.Statement,
		lesson.Scope.Service, lesson.Scope.Env, lesson.Scope.Team,
		lesson.Trigger, lesson.Kind,
		lesson.Confidence,
		lesson.DerivedAt.UTC(),
		lastVerified,
		source,
		actorOr(lesson.CreatedBy),
		string(lesson.SubjectClass),
	)
	if err != nil {
		return fmt.Errorf("insert lesson: %w", err)
	}
	if len(lesson.Evidence) > 0 {
		if err := r.AppendEvidence(ctx, lesson.ID, lesson.Evidence); err != nil {
			return err
		}
	}
	return nil
}

// GetByID returns the lesson plus its evidence.
func (r LessonRepository) GetByID(ctx context.Context, id string) (domain.Lesson, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, statement, scope_service, scope_env, scope_team, `+"`trigger`"+`, kind, confidence, derived_at, last_verified, source, created_by, subject_class
FROM lessons WHERE id = ?`, id)
	l, err := scanLessonRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Lesson{}, fmt.Errorf("lesson %s not found", id)
	}
	if err != nil {
		return domain.Lesson{}, err
	}
	ev, err := r.ListEvidence(ctx, id)
	if err != nil {
		return domain.Lesson{}, err
	}
	l.Evidence = ev
	return l, nil
}

// ListByService returns lessons scoped to the given service.
func (r LessonRepository) ListByService(ctx context.Context, service string) ([]domain.Lesson, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, statement, scope_service, scope_env, scope_team, `+"`trigger`"+`, kind, confidence, derived_at, last_verified, source, created_by, subject_class
FROM lessons WHERE scope_service = ? ORDER BY confidence DESC, derived_at DESC`, service)
	if err != nil {
		return nil, fmt.Errorf("list lessons by service: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return r.collectLessonRows(ctx, rows)
}

// ListByTrigger returns lessons matching a trigger label.
func (r LessonRepository) ListByTrigger(ctx context.Context, trigger string) ([]domain.Lesson, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, statement, scope_service, scope_env, scope_team, `+"`trigger`"+`, kind, confidence, derived_at, last_verified, source, created_by, subject_class
FROM lessons WHERE `+"`trigger`"+` = ? ORDER BY confidence DESC, derived_at DESC`, trigger)
	if err != nil {
		return nil, fmt.Errorf("list lessons by trigger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return r.collectLessonRows(ctx, rows)
}

// ListAll returns every lesson, highest confidence first.
func (r LessonRepository) ListAll(ctx context.Context) ([]domain.Lesson, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, statement, scope_service, scope_env, scope_team, `+"`trigger`"+`, kind, confidence, derived_at, last_verified, source, created_by, subject_class
FROM lessons ORDER BY confidence DESC, derived_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list all lessons: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return r.collectLessonRows(ctx, rows)
}

// CountAll returns the total number of lessons stored.
func (r LessonRepository) CountAll(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lessons`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count lessons: %w", err)
	}
	return n, nil
}

// DeleteAll wipes lessons and lesson_evidence.
func (r LessonRepository) DeleteAll(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM lesson_evidence`); err != nil {
		return fmt.Errorf("delete all lesson evidence: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM lessons`); err != nil {
		return fmt.Errorf("delete all lessons: %w", err)
	}
	return nil
}

// AppendEvidence is idempotent on (lesson_id, action_id).
func (r LessonRepository) AppendEvidence(ctx context.Context, lessonID string, actionIDs []string) error {
	for _, aid := range actionIDs {
		if _, err := r.db.ExecContext(ctx, `
INSERT IGNORE INTO lesson_evidence (lesson_id, action_id) VALUES (?, ?)`, lessonID, aid); err != nil {
			return fmt.Errorf("append lesson evidence: %w", err)
		}
	}
	return nil
}

// ListEvidence returns the action ids backing a given lesson.
func (r LessonRepository) ListEvidence(ctx context.Context, lessonID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT action_id FROM lesson_evidence WHERE lesson_id = ? ORDER BY action_id`,
		lessonID,
	)
	if err != nil {
		return nil, fmt.Errorf("list lesson evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0)
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			return nil, err
		}
		out = append(out, aid)
	}
	return out, rows.Err()
}

func (r LessonRepository) collectLessonRows(ctx context.Context, rows *sql.Rows) ([]domain.Lesson, error) {
	out := make([]domain.Lesson, 0)
	for rows.Next() {
		l, err := scanLessonRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, r.hydrateEvidence(ctx, out)
}

// evidenceHydrationChunk bounds the IN list so one enormous lesson set does not
// exceed max_allowed_packet with a single giant statement.
const evidenceHydrationChunk = 500

// hydrateEvidence fills in Evidence for every lesson using ONE query per chunk
// instead of one per lesson. ListAll/ListByService/ListByTrigger previously
// issued a round trip per returned row, so listing cost grew linearly with a
// lesson corpus that only ever grows (synthesis adds, nothing removes).
func (r LessonRepository) hydrateEvidence(ctx context.Context, lessons []domain.Lesson) error {
	if len(lessons) == 0 {
		return nil
	}
	byLesson := make(map[string][]string, len(lessons))
	for start := 0; start < len(lessons); start += evidenceHydrationChunk {
		end := min(start+evidenceHydrationChunk, len(lessons))
		chunk := lessons[start:end]
		args := make([]any, len(chunk))
		for i, l := range chunk {
			args[i] = l.ID
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		//nolint:gosec // G202: placeholders are literal "?" tokens, not user input
		rows, err := r.db.QueryContext(ctx,
			`SELECT lesson_id, action_id FROM lesson_evidence WHERE lesson_id IN (`+
				placeholders+`) ORDER BY lesson_id, action_id`, args...)
		if err != nil {
			return fmt.Errorf("list lesson evidence: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var lessonID, actionID string
				if err := rows.Scan(&lessonID, &actionID); err != nil {
					return fmt.Errorf("scan lesson evidence: %w", err)
				}
				byLesson[lessonID] = append(byLesson[lessonID], actionID)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
	}
	for i := range lessons {
		ev, ok := byLesson[lessons[i].ID]
		if !ok {
			// Match the per-lesson path, which returned an empty non-nil slice.
			ev = []string{}
		}
		lessons[i].Evidence = ev
	}
	return nil
}

// ListVersions returns every snapshot row for the given lesson.
func (r LessonRepository) ListVersions(ctx context.Context, lessonID string) ([]ports.EntityVersion, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT version_id, payload_json, valid_from, valid_to
FROM lesson_versions WHERE lesson_id = ? ORDER BY version_id DESC`, lessonID)
	if err != nil {
		return nil, fmt.Errorf("list lesson versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]ports.EntityVersion, 0)
	for rows.Next() {
		var v ports.EntityVersion
		if err := rows.Scan(&v.VersionID, &v.PayloadJSON, &v.ValidFrom, &v.ValidTo); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r LessonRepository) snapshotIfExists(ctx context.Context, lessonID string) error {
	current, err := r.GetByID(ctx, lessonID)
	if err != nil {
		return nil //nolint:nilerr // missing prior row = no snapshot
	}
	payload, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshal lesson snapshot: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO lesson_versions (lesson_id, payload_json, valid_from, valid_to)
VALUES (?, CAST(? AS JSON), ?, ?)`,
		lessonID, string(payload), current.DerivedAt.UTC(), time.Now().UTC(),
	)
	return err
}

func scanLessonRow(row *sql.Row) (domain.Lesson, error) {
	var l domain.Lesson
	var lastVerified sql.NullTime
	var subjectClass string
	if err := row.Scan(
		&l.ID, &l.Statement,
		&l.Scope.Service, &l.Scope.Env, &l.Scope.Team,
		&l.Trigger, &l.Kind,
		&l.Confidence,
		&l.DerivedAt, &lastVerified,
		&l.Source, &l.CreatedBy,
		&subjectClass,
	); err != nil {
		return domain.Lesson{}, err
	}
	if lastVerified.Valid {
		l.LastVerified = lastVerified.Time
	}
	l.SubjectClass = domain.SubjectClass(subjectClass)
	return l, nil
}

func scanLessonRows(rows *sql.Rows) (domain.Lesson, error) {
	var l domain.Lesson
	var lastVerified sql.NullTime
	var subjectClass string
	if err := rows.Scan(
		&l.ID, &l.Statement,
		&l.Scope.Service, &l.Scope.Env, &l.Scope.Team,
		&l.Trigger, &l.Kind,
		&l.Confidence,
		&l.DerivedAt, &lastVerified,
		&l.Source, &l.CreatedBy,
		&subjectClass,
	); err != nil {
		return domain.Lesson{}, err
	}
	if lastVerified.Valid {
		l.LastVerified = lastVerified.Time
	}
	l.SubjectClass = domain.SubjectClass(subjectClass)
	return l, nil
}

var _ = time.Now // keep import non-empty if helpers grow
