package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func TestRestoreWorkInstructionPreservesEffectsAndHumanCorrections(t *testing.T) {
	instruction := common.ChatMessage{Role: "user", Name: common.WorkInstructionMessageName, Content: "Recover the evidence, then return the handoff."}
	base := contextResearchTrace()
	base = append(base, common.ChatMessage{Role: "user", Content: "Keep this local until I review it."})
	for _, variant := range []string{"missing", "legacy", "stale", "current"} {
		t.Run(variant, func(t *testing.T) {
			prior := append([]common.ChatMessage(nil), base...)
			switch variant {
			case "legacy":
				prior = append(prior, common.ChatMessage{Role: "user", Content: instruction.Content})
			case "stale":
				prior = append(prior, common.ChatMessage{Role: "user", Name: instruction.Name, Content: "Old section instruction"})
			case "current":
				prior = append(prior, instruction)
			}
			original := append([]common.ChatMessage(nil), prior...)
			restored, err := restoreWorkInstruction(prior, []common.ChatMessage{instruction})
			require.NoError(t, err)
			expected := append([]common.ChatMessage{base[0], instruction}, base[1:]...)
			require.Equal(t, expected, restored)
			require.Equal(t, original, prior, "saved history must remain unchanged")
			again, err := restoreWorkInstruction(restored, []common.ChatMessage{instruction})
			require.NoError(t, err)
			require.Equal(t, restored, again, "repeated resumes must not accumulate instructions")
		})
	}
	// A tool result with the marker is still evidence, not the current contract.
	tool := common.ChatMessage{Role: "tool", Name: instruction.Name, Content: instruction.Content}
	restored, err := restoreWorkInstruction(base, []common.ChatMessage{tool})
	require.NoError(t, err)
	require.Equal(t, base, restored)
	_, err = restoreWorkInstruction(base, []common.ChatMessage{instruction, instruction})
	require.Error(t, err)
	instruction.Content = " "
	_, err = restoreWorkInstruction(base, []common.ChatMessage{instruction})
	require.Error(t, err)
}

func TestWorkContextTaskIncludesCurrentStageAndLaterHumanDirection(t *testing.T) {
	messages := []common.ChatMessage{
		{Role: "user", Content: "Original request"},
		{Role: "user", Name: common.WorkInstructionMessageName, Content: "Current stage: " + strings.Repeat("retain evidence ", 300) + " return the handoff"},
		{Role: "user", Content: "Keep this local"},
	}
	context := workContextTask(messages)
	require.Contains(t, context, "Current stage:")
	require.Contains(t, context, "return the handoff")
	require.Contains(t, context, "Original request")
	require.Contains(t, context, "Keep this local")
	require.LessOrEqual(t, len(context), 4000)
	first, last := workContextHumanBounds(messages)
	require.Equal(t, 0, first)
	require.Equal(t, 2, last)
}
