package relay

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// allTools mirrors the registration list in New() — keep both in sync.
func allTools() []mcp.Tool {
	return []mcp.Tool{
		whoamiTool(), registerAgentTool(), sendMessageTool(), getInboxTool(), ackDeliveryTool(),
		getThreadTool(), listAgentsTool(), markReadTool(), createConversationTool(), listConversationsTool(),
		getConversationMessagesTool(), inviteToConversationTool(), leaveConversationTool(), archiveConversationTool(),
		setMemoryTool(), getMemoryTool(), searchMemoryTool(), listMemoriesTool(), deleteMemoryTool(), resolveConflictTool(),
		registerProfileTool(), getProfileTool(), listProfilesTool(), findProfilesTool(),
		dispatchTaskTool(), claimTaskTool(), startTaskTool(), completeTaskTool(), blockTaskTool(), resumeTaskTool(), cancelTaskTool(),
		getTaskTool(), listTasksTool(), updateTaskTool(), archiveTasksTool(), moveTaskTool(),
		batchCompleteTasksTool(), batchDispatchTasksTool(),
		createBoardTool(), listBoardsTool(), archiveBoardTool(), deleteBoardTool(),
		createGoalTool(), listGoalsTool(), getGoalTool(), updateGoalTool(), getGoalCascadeTool(),
		claimFilesTool(), releaseFilesTool(), listLocksTool(),
		deactivateAgentTool(), deleteAgentTool(), sleepAgentTool(),
		createProjectTool(), deleteProjectTool(),
		queryContextTool(), getSessionContextTool(),
		createOrgTool(), listOrgsTool(), createTeamTool(), listTeamsTool(), addTeamMemberTool(), removeTeamMemberTool(),
		getTeamInboxTool(), addNotifyChannelTool(),
	}
}

// Every connected agent pays the serialized tool list in context tokens at
// session start. This budget blocks silent regressions: if a new tool or a
// fatter description pushes the total over the cap, trim descriptions or
// raise the cap deliberately in the same PR.
const toolSchemaBudgetBytes = 48000

func TestToolSchemaBudget(t *testing.T) {
	total := 0
	for _, tool := range allTools() {
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal %s: %v", tool.Name, err)
		}
		total += len(b)
		if len(b) > 2300 {
			t.Errorf("tool %s schema is %d bytes (max 2300) — trim its descriptions", tool.Name, len(b))
		}
	}
	t.Logf("tool schemas: %d tools, %d bytes (~%d tokens)", len(allTools()), total, total/4)
	if total > toolSchemaBudgetBytes {
		t.Errorf("total tool schema size %d bytes exceeds budget %d — trim descriptions", total, toolSchemaBudgetBytes)
	}
}
