package common

// WorkInstructionMessageName identifies the current native execution's prompt.
// It is a retention marker, not a message role, permission, or proof of effects.
// The executing automation supplies its content; ordinary conversation history
// and recalled sources must not manufacture the active step contract.
const WorkInstructionMessageName = "memql_work_instruction"

func IsWorkInstruction(message ChatMessage) bool {
	return message.Role == "user" && message.Name == WorkInstructionMessageName
}
