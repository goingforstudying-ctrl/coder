package agentapi

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/google/uuid"
	"golang.org/x/xerrors"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"cdr.dev/slog/v3"
	agentproto "github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/quartz"
)

// MaxContextSchemaVersion is the highest on-wire schema_version the
// coderd context push handler understands. Pushes carrying a higher
// value are rejected with an explicit error so a forward incompatible
// agent fails loudly during rollout instead of silently degrading.
//
// Bump this whenever the wire shape of the context proto changes in
// a way the handler must opt into; bumping it without coordinated
// code changes breaks every newer agent.
const MaxContextSchemaVersion uint64 = 1

// ContextAPI implements the v2.10 PushContextState RPC. It persists
// the latest pushed snapshot per workspace agent across two tables
// (workspace_agent_context_snapshots and
// workspace_agent_context_resources) so later phases can hydrate
// chats and surface drift to the dashboard.
//
// The handler is a pure write path: nothing else in coderd reads
// these rows yet. If a bug here returns errors the agent's RunPush
// loop backs off and the workspace keeps behaving exactly like it
// did before v2.10.
type ContextAPI struct {
	AgentID  uuid.UUID
	Log      slog.Logger
	Clock    quartz.Clock
	Database database.Store
}

// PushContextState persists a snapshot pushed by the workspace
// agent. The single transaction upserts the snapshot row, upserts
// each resource, then deletes any resources whose source is not in
// the incoming set so the stored snapshot and resource table always
// agree.
//
// Returns accepted = false (without writing) when the push is a
// replay or out-of-order resend: the agent's per-process version
// counter is monotonic, and only an initial = true push from a
// freshly-booted agent resets that baseline. Replays and stale
// retransmits leave the stored state untouched.
func (a *ContextAPI) PushContextState(ctx context.Context, req *agentproto.PushContextStateRequest) (*agentproto.PushContextStateResponse, error) {
	if req == nil {
		return nil, xerrors.New("agentapi: PushContextState request is nil")
	}
	if req.SchemaVersion > MaxContextSchemaVersion {
		return nil, xerrors.Errorf("agentapi: PushContextState schema_version %d exceeds supported maximum %d", req.SchemaVersion, MaxContextSchemaVersion)
	}

	rows, err := validateAndConvertContextResources(req.Resources)
	if err != nil {
		return nil, err
	}

	//nolint:gocritic // The push handler runs in the agent connection's authenticated context; the agent token role does not own the workspace_agent_context resource. We elevate to a narrow agent-context subject for the upsert + prune.
	ctx = dbauthz.AsAgentContext(ctx)

	clock := a.Clock
	if clock == nil {
		clock = quartz.NewReal()
	}
	now := dbtime.Time(clock.Now())

	activeSources := make([]string, 0, len(rows))
	for _, r := range rows {
		activeSources = append(activeSources, r.source)
	}
	sort.Strings(activeSources)

	var accepted bool
	err = a.Database.InTx(func(tx database.Store) error {
		existing, err := tx.GetLatestWorkspaceAgentContextSnapshot(ctx, a.AgentID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// No previous snapshot; first push always wins.
		case err != nil:
			return xerrors.Errorf("get latest snapshot: %w", err)
		default:
			// Accept either a fresh agent process (initial) or
			// a strictly newer version. Out-of-order or replayed
			// pushes leave the stored state untouched.
			//
			//nolint:gosec // existing.Version is a uint64 round-tripped via BIGINT; non-negative by construction.
			if !req.Initial && req.Version <= uint64(existing.Version) {
				return nil
			}
		}

		_, err = tx.UpsertWorkspaceAgentContextSnapshot(ctx, database.UpsertWorkspaceAgentContextSnapshotParams{
			WorkspaceAgentID: a.AgentID,
			//nolint:gosec // Agent push counter; would take ~292M years at 1 push/ms to overflow int64.
			Version: int64(req.Version),
			//nolint:gosec // SchemaVersion is the wire schema version, single digits in practice.
			SchemaVersion: int64(req.SchemaVersion),
			AggregateHash: append([]byte(nil), req.AggregateHash...),
			SnapshotError: req.SnapshotError,
			ReceivedAt:    now,
		})
		if err != nil {
			return xerrors.Errorf("upsert snapshot: %w", err)
		}

		for _, r := range rows {
			_, err = tx.UpsertWorkspaceAgentContextResource(ctx, database.UpsertWorkspaceAgentContextResourceParams{
				WorkspaceAgentID: a.AgentID,
				Source:           r.source,
				BodyKind:         r.bodyKind,
				Body:             r.body,
				ContentHash:      append([]byte(nil), r.contentHash...),
				//nolint:gosec // SizeBytes is bounded by the agent's 64KiB per-resource cap and the 2MiB aggregate cap.
				SizeBytes:  int64(r.sizeBytes),
				Status:     r.status,
				Error:      r.errorMsg,
				SourcePath: r.sourcePath,
				Now:        now,
			})
			if err != nil {
				return xerrors.Errorf("upsert resource %q: %w", r.source, err)
			}
		}

		err = tx.DeleteStaleWorkspaceAgentContextResources(ctx, database.DeleteStaleWorkspaceAgentContextResourcesParams{
			WorkspaceAgentID: a.AgentID,
			ActiveSources:    activeSources,
		})
		if err != nil {
			return xerrors.Errorf("delete stale resources: %w", err)
		}

		accepted = true
		return nil
	}, &database.TxOptions{TxIdentifier: "push_agent_context_state"})
	if err != nil {
		return nil, err
	}

	if !accepted {
		a.Log.Debug(ctx, "PushContextState dropped: replay or out-of-order",
			slog.F("agent_id", a.AgentID),
			slog.F("version", req.Version),
			slog.F("initial", req.Initial),
		)
		return &agentproto.PushContextStateResponse{Accepted: false}, nil
	}

	a.Log.Debug(ctx, "PushContextState accepted",
		slog.F("agent_id", a.AgentID),
		slog.F("version", req.Version),
		slog.F("schema_version", req.SchemaVersion),
		slog.F("initial", req.Initial),
		slog.F("resources", len(rows)),
	)
	return &agentproto.PushContextStateResponse{Accepted: true}, nil
}

// resourceRow is the validated, ready-to-persist form of a single
// incoming ContextResource. It collapses the proto oneof to (kind,
// JSONB) so the DB upsert is uniform across kinds.
type resourceRow struct {
	source      string
	sourcePath  string
	bodyKind    string
	body        []byte
	contentHash []byte
	sizeBytes   uint64
	status      string
	errorMsg    string
}

// validateAndConvertContextResources translates wire resources into
// resourceRow values while rejecting structurally invalid input:
//
//   - empty or duplicate sources (the PK depends on uniqueness),
//   - unknown body variants (kept extensible by emitting the proto's
//     reserved kinds via dedicated body messages),
//   - unknown status enum values.
//
// Validation is deliberately strict here so a misbehaving agent
// cannot poison the snapshot table. Phase 2 readers can then trust
// that every row maps to a known proto variant.
func validateAndConvertContextResources(resources []*agentproto.ContextResource) ([]resourceRow, error) {
	rows := make([]resourceRow, 0, len(resources))
	seen := make(map[string]struct{}, len(resources))
	for i, r := range resources {
		if r == nil {
			return nil, xerrors.Errorf("agentapi: PushContextState resource at index %d is nil", i)
		}
		if r.Source == "" {
			return nil, xerrors.Errorf("agentapi: PushContextState resource at index %d has empty source", i)
		}
		if _, ok := seen[r.Source]; ok {
			return nil, xerrors.Errorf("agentapi: PushContextState duplicate source %q", r.Source)
		}
		seen[r.Source] = struct{}{}

		kind, body, err := marshalContextResourceBody(r)
		if err != nil {
			return nil, xerrors.Errorf("resource %q: %w", r.Source, err)
		}
		status, err := contextResourceStatusToString(r.Status)
		if err != nil {
			return nil, xerrors.Errorf("resource %q: %w", r.Source, err)
		}

		rows = append(rows, resourceRow{
			source:      r.Source,
			sourcePath:  r.GetSourcePath(),
			bodyKind:    kind,
			body:        body,
			contentHash: r.ContentHash,
			sizeBytes:   r.SizeBytes,
			status:      status,
			errorMsg:    r.Error,
		})
	}
	return rows, nil
}

// marshalContextResourceBody picks the body variant set on the wire
// resource and returns the (body_kind, body_jsonb) pair stored in
// the resource row. The body is protojson encoded so the schema can
// be evolved by adding fields to the proto without coderd changes,
// and a future reader can round-trip back to the proto type by
// switching on body_kind.
//
// Body is always populated, even on non-OK statuses: the wire
// guarantees the oneof variant is set so coderd can still attribute
// the failure to a known kind. For variants with no content fields
// (mcp_config), an empty JSON object is stored.
func marshalContextResourceBody(r *agentproto.ContextResource) (kind string, body []byte, err error) {
	switch b := r.Body.(type) {
	case *agentproto.ContextResource_InstructionFile:
		payload := b.InstructionFile
		if payload == nil {
			payload = &agentproto.InstructionFileBody{}
		}
		body, err = marshalBody(payload)
		return "instruction_file", body, err
	case *agentproto.ContextResource_Skill:
		payload := b.Skill
		if payload == nil {
			payload = &agentproto.SkillMetaBody{}
		}
		body, err = marshalBody(payload)
		return "skill", body, err
	case *agentproto.ContextResource_McpConfig:
		payload := b.McpConfig
		if payload == nil {
			payload = &agentproto.MCPConfigBody{}
		}
		body, err = marshalBody(payload)
		return "mcp_config", body, err
	case *agentproto.ContextResource_McpServer:
		payload := b.McpServer
		if payload == nil {
			payload = &agentproto.MCPServerBody{}
		}
		body, err = marshalBody(payload)
		return "mcp_server", body, err
	case nil:
		return "", nil, xerrors.Errorf("missing body variant; status %s requires a typed body", r.Status)
	default:
		return "", nil, xerrors.Errorf("unsupported body variant %T", r.Body)
	}
}

// contextBodyMarshalOptions produces deterministic-ish JSON for the
// body so the stored value compares equal across pushes that yield
// equivalent protos. Strict canonicalization (RFC 8785) is not
// required here; the CHECK constraint plus the protojson round trip
// give us a stable enough store.
var contextBodyMarshalOptions = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: false,
}

// marshalBody is a small wrapper around protojson.Marshal that
// keeps the body encoding in one place; future phases that read
// these rows mirror the call with protojson.Unmarshal into the
// matching proto.Message.
func marshalBody(msg proto.Message) ([]byte, error) {
	out, err := contextBodyMarshalOptions.Marshal(msg)
	if err != nil {
		return nil, xerrors.Errorf("marshal body: %w", err)
	}
	return out, nil
}

// contextResourceStatusToString translates the wire status enum to
// the on-disk CHECK constraint label. STATUS_UNSPECIFIED is
// rejected: every well-formed snapshot row needs an explicit status
// so cache invalidation, dirty fan-out, and the Sources drawer can
// reason about partial pushes deterministically.
func contextResourceStatusToString(s agentproto.ContextResource_Status) (string, error) {
	switch s {
	case agentproto.ContextResource_OK:
		return "ok", nil
	case agentproto.ContextResource_OVERSIZE:
		return "oversize", nil
	case agentproto.ContextResource_UNREADABLE:
		return "unreadable", nil
	case agentproto.ContextResource_INVALID:
		return "invalid", nil
	case agentproto.ContextResource_EXCLUDED:
		return "excluded", nil
	default:
		return "", xerrors.Errorf("unknown status %d", s)
	}
}
