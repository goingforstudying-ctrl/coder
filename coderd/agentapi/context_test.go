package agentapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/slogtest"
	agentproto "github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/coderd/agentapi"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbmock"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/quartz"
)

func TestPushContextState(t *testing.T) {
	t.Parallel()

	now := dbtime.Time(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	agentID := uuid.New()
	clock := quartz.NewMock(t)
	clock.Set(now)

	makeAPI := func(t *testing.T) (*agentapi.ContextAPI, *dbmock.MockStore) {
		t.Helper()
		ctrl := gomock.NewController(t)
		dbm := dbmock.NewMockStore(ctrl)
		return &agentapi.ContextAPI{
			AgentID:  agentID,
			Log:      slogtest.Make(t, &slogtest.Options{IgnoreErrors: true}).Leveled(slog.LevelDebug),
			Clock:    clock,
			Database: dbm,
		}, dbm
	}

	// expectInTx wires the dbmock so InTx invokes the closure on the
	// same mock; tests then set per-method expectations on the same
	// dbm.
	expectInTx := func(dbm *dbmock.MockStore) {
		dbm.EXPECT().InTx(gomock.Any(), gomock.Any()).Times(1).DoAndReturn(
			func(f func(database.Store) error, _ *database.TxOptions) error {
				return f(dbm)
			},
		)
	}

	t.Run("AcceptsInitialPush", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{}, errNoRows())
		dbm.EXPECT().UpsertWorkspaceAgentContextSnapshot(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextSnapshot{}, nil)
		dbm.EXPECT().UpsertWorkspaceAgentContextResource(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextResource{}, nil).Times(2)
		dbm.EXPECT().DeleteStaleWorkspaceAgentContextResources(gomock.Any(), database.DeleteStaleWorkspaceAgentContextResourcesParams{
			WorkspaceAgentID: agentID,
			ActiveSources:    []string{"/home/coder/.mcp.json", "/home/coder/AGENTS.md"},
		}).Return(nil)

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version:       1,
			AggregateHash: []byte{0x01, 0x02, 0x03},
			Initial:       true,
			Resources: []*agentproto.ContextResource{
				instructionResource("/home/coder/AGENTS.md", "hello"),
				mcpConfigResource("/home/coder/.mcp.json"),
			},
		})
		require.NoError(t, err)
		require.True(t, resp.GetAccepted())
	})

	t.Run("RejectsEmptyAndDuplicateSources", func(t *testing.T) {
		t.Parallel()

		t.Run("Empty", func(t *testing.T) {
			t.Parallel()
			api, _ := makeAPI(t)
			resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
				Version: 1,
				Initial: true,
				Resources: []*agentproto.ContextResource{
					instructionResource("", "x"),
				},
			})
			require.Error(t, err)
			require.Nil(t, resp)
			require.Contains(t, err.Error(), "empty source")
		})

		t.Run("Duplicate", func(t *testing.T) {
			t.Parallel()
			api, _ := makeAPI(t)
			resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
				Version: 1,
				Initial: true,
				Resources: []*agentproto.ContextResource{
					instructionResource("/a", "x"),
					instructionResource("/a", "y"),
				},
			})
			require.Error(t, err)
			require.Nil(t, resp)
			require.Contains(t, err.Error(), "duplicate source")
		})
	})

	t.Run("RejectsUnknownStatus", func(t *testing.T) {
		t.Parallel()

		api, _ := makeAPI(t)
		// STATUS_UNSPECIFIED is the zero value and must be rejected so
		// every persisted row has a meaningful status.
		resource := instructionResource("/a", "x")
		resource.Status = agentproto.ContextResource_STATUS_UNSPECIFIED
		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version:   1,
			Initial:   true,
			Resources: []*agentproto.ContextResource{resource},
		})
		require.Error(t, err)
		require.Nil(t, resp)
	})

	t.Run("RejectsMissingBody", func(t *testing.T) {
		t.Parallel()

		api, _ := makeAPI(t)
		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 1,
			Initial: true,
			Resources: []*agentproto.ContextResource{
				{
					Source:      "/a",
					ContentHash: []byte{0x01},
					Status:      agentproto.ContextResource_OK,
					// Body deliberately unset.
				},
			},
		})
		require.Error(t, err)
		require.Nil(t, resp)
		require.Contains(t, err.Error(), "missing body")
	})

	t.Run("StaleVersionDropped", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		// Existing version 5 stored; incoming version 3 with initial=false
		// is a replay/out-of-order push and must be silently dropped
		// (accepted=false) without writing.
		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{Version: 5}, nil)

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 3,
			Initial: false,
			Resources: []*agentproto.ContextResource{
				instructionResource("/a", "stale"),
			},
		})
		require.NoError(t, err)
		require.False(t, resp.GetAccepted())
	})

	t.Run("SameVersionReplayDropped", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{Version: 5}, nil)

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 5,
			Initial: false,
		})
		require.NoError(t, err)
		require.False(t, resp.GetAccepted())
	})

	t.Run("InitialOverwritesLowerVersion", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		// Agent rebooted: in-memory counter back to 1 but the stored
		// version from the previous process boot is 5. initial=true is
		// authoritative and the push is accepted.
		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{Version: 5}, nil)
		dbm.EXPECT().UpsertWorkspaceAgentContextSnapshot(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextSnapshot{}, nil)
		dbm.EXPECT().UpsertWorkspaceAgentContextResource(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextResource{}, nil)
		dbm.EXPECT().DeleteStaleWorkspaceAgentContextResources(gomock.Any(), gomock.Any()).
			Return(nil)

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 1,
			Initial: true,
			Resources: []*agentproto.ContextResource{
				instructionResource("/a", "fresh"),
			},
		})
		require.NoError(t, err)
		require.True(t, resp.GetAccepted())
	})

	t.Run("PrunesStaleResources", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{Version: 1}, nil)
		dbm.EXPECT().UpsertWorkspaceAgentContextSnapshot(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextSnapshot{}, nil)
		dbm.EXPECT().UpsertWorkspaceAgentContextResource(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextResource{}, nil)
		// Even with one active resource the prune call still runs so
		// any resource not in the active set is removed in the same
		// transaction.
		dbm.EXPECT().DeleteStaleWorkspaceAgentContextResources(gomock.Any(), database.DeleteStaleWorkspaceAgentContextResourcesParams{
			WorkspaceAgentID: agentID,
			ActiveSources:    []string{"/a"},
		}).Return(nil)

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 2,
			Initial: false,
			Resources: []*agentproto.ContextResource{
				instructionResource("/a", "still here"),
			},
		})
		require.NoError(t, err)
		require.True(t, resp.GetAccepted())
	})

	t.Run("EmptyResourceListAcceptedAndPrunesAll", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{}, errNoRows())
		dbm.EXPECT().UpsertWorkspaceAgentContextSnapshot(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextSnapshot{}, nil)
		// Active sources is an explicitly empty slice (not nil) so the
		// generated SQL deletes every row for this agent rather than
		// no-oping on a NULL array.
		dbm.EXPECT().DeleteStaleWorkspaceAgentContextResources(gomock.Any(), database.DeleteStaleWorkspaceAgentContextResourcesParams{
			WorkspaceAgentID: agentID,
			ActiveSources:    []string{},
		}).Return(nil)

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 1,
			Initial: true,
		})
		require.NoError(t, err)
		require.True(t, resp.GetAccepted())
	})

	t.Run("PersistsAllKnownBodyVariants", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{}, errNoRows())
		dbm.EXPECT().UpsertWorkspaceAgentContextSnapshot(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextSnapshot{}, nil)

		gotKinds := map[string][]byte{}
		dbm.EXPECT().UpsertWorkspaceAgentContextResource(gomock.Any(), gomock.Any()).
			Times(4).
			DoAndReturn(func(_ context.Context, arg database.UpsertWorkspaceAgentContextResourceParams) (database.WorkspaceAgentContextResource, error) {
				gotKinds[arg.BodyKind] = arg.Body
				return database.WorkspaceAgentContextResource{}, nil
			})

		dbm.EXPECT().DeleteStaleWorkspaceAgentContextResources(gomock.Any(), gomock.Any()).Return(nil)

		mcpServer := mcpServerResource("/srv/mcp/echo", "echo", "echo server")
		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version: 1,
			Initial: true,
			Resources: []*agentproto.ContextResource{
				instructionResource("/a/AGENTS.md", "hi"),
				skillResource("/a/.agents/skills/example/SKILL.md", "example", "an example"),
				mcpConfigResource("/a/.mcp.json"),
				mcpServer,
			},
		})
		require.NoError(t, err)
		require.True(t, resp.GetAccepted())

		require.Contains(t, gotKinds, "instruction_file")
		require.Contains(t, gotKinds, "skill")
		require.Contains(t, gotKinds, "mcp_config")
		require.Contains(t, gotKinds, "mcp_server")

		// Confirm each body deserializes as JSON; the actual proto
		// roundtrip is exercised by the resolver tests on the agent
		// side. We just sanity-check the encoding here.
		for kind, body := range gotKinds {
			var raw map[string]any
			err := json.Unmarshal(body, &raw)
			require.NoErrorf(t, err, "kind %q body not valid JSON: %s", kind, string(body))
		}
	})

	t.Run("NonOKStatusStillPersisted", func(t *testing.T) {
		t.Parallel()

		api, dbm := makeAPI(t)
		expectInTx(dbm)

		dbm.EXPECT().GetLatestWorkspaceAgentContextSnapshot(gomock.Any(), agentID).
			Return(database.WorkspaceAgentContextSnapshot{}, errNoRows())
		dbm.EXPECT().UpsertWorkspaceAgentContextSnapshot(gomock.Any(), gomock.Any()).
			Return(database.WorkspaceAgentContextSnapshot{}, nil)

		var got database.UpsertWorkspaceAgentContextResourceParams
		dbm.EXPECT().UpsertWorkspaceAgentContextResource(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, arg database.UpsertWorkspaceAgentContextResourceParams) (database.WorkspaceAgentContextResource, error) {
				got = arg
				return database.WorkspaceAgentContextResource{}, nil
			})
		dbm.EXPECT().DeleteStaleWorkspaceAgentContextResources(gomock.Any(), gomock.Any()).Return(nil)

		oversized := instructionResource("/a/AGENTS.md", "")
		oversized.Status = agentproto.ContextResource_OVERSIZE
		oversized.SizeBytes = 65 * 1024
		oversized.Error = "file exceeds 64KiB per-resource cap"

		resp, err := api.PushContextState(context.Background(), &agentproto.PushContextStateRequest{
			Version:   1,
			Initial:   true,
			Resources: []*agentproto.ContextResource{oversized},
		})
		require.NoError(t, err)
		require.True(t, resp.GetAccepted())
		require.Equal(t, "instruction_file", got.BodyKind)
		require.Equal(t, "oversize", got.Status)
		require.Equal(t, int64(65*1024), got.SizeBytes)
		require.Equal(t, "file exceeds 64KiB per-resource cap", got.Error)
	})
}

// errNoRows returns the database "no rows" sentinel for the mocks;
// the handler uses errors.Is(err, sql.ErrNoRows) to recognize first
// pushes vs. updates.
func errNoRows() error {
	return sql.ErrNoRows
}

func instructionResource(source, content string) *agentproto.ContextResource {
	return &agentproto.ContextResource{
		Source:      source,
		ContentHash: []byte{0xaa, 0xbb, 0xcc},
		Status:      agentproto.ContextResource_OK,
		SizeBytes:   uint64(len(content)),
		Body: &agentproto.ContextResource_InstructionFile{
			InstructionFile: &agentproto.InstructionFileBody{
				Content: []byte(content),
			},
		},
	}
}

func skillResource(source, name, description string) *agentproto.ContextResource {
	return &agentproto.ContextResource{
		Source:      source,
		ContentHash: []byte{0x01, 0x02, 0x03},
		Status:      agentproto.ContextResource_OK,
		Body: &agentproto.ContextResource_Skill{
			Skill: &agentproto.SkillMetaBody{
				Meta:        []byte("---\nname: " + name + "\n---\nbody"),
				Name:        name,
				Description: description,
			},
		},
	}
}

func mcpConfigResource(source string) *agentproto.ContextResource {
	return &agentproto.ContextResource{
		Source:      source,
		ContentHash: []byte{0xde, 0xad, 0xbe, 0xef},
		Status:      agentproto.ContextResource_OK,
		Body: &agentproto.ContextResource_McpConfig{
			McpConfig: &agentproto.MCPConfigBody{},
		},
	}
}

func mcpServerResource(source, serverName, description string) *agentproto.ContextResource {
	return &agentproto.ContextResource{
		Source:      source,
		ContentHash: []byte{0x10, 0x20, 0x30},
		Status:      agentproto.ContextResource_OK,
		Body: &agentproto.ContextResource_McpServer{
			McpServer: &agentproto.MCPServerBody{
				ServerName:  serverName,
				Description: description,
			},
		},
	}
}
