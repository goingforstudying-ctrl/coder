package chatd

import (
	"context"
	"sync"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
)

type TurnWorkspaceContextForTest struct {
	inner *turnWorkspaceContext
}

func NewTurnWorkspaceContextForTest(server *Server, chat database.Chat) *TurnWorkspaceContextForTest {
	return &TurnWorkspaceContextForTest{inner: &turnWorkspaceContext{
		server:           server,
		chatStateMu:      &sync.Mutex{},
		currentChat:      &chat,
		loadChatSnapshot: server.db.GetChatByID,
	}}
}

func (c *TurnWorkspaceContextForTest) GetWorkspaceConn(ctx context.Context) (workspacesdk.AgentConn, error) {
	return c.inner.getWorkspaceConn(ctx)
}
