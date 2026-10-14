package common

// UnexecutedWorkToolResult pairs a proposed native tool call before it starts.
// Checkpoint readers use this exact protocol marker; it is not an effect receipt.
const UnexecutedWorkToolResult = "Not executed before the saved checkpoint. Inspect recorded results and any human answer before continuing."
