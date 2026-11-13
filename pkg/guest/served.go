//go:build !disco_vm_older_agent

package guest

// servedProtocol is the protocol the agent reports in its info: Protocol,
// except in a build tagged disco_vm_older_agent (served_older.go).
const servedProtocol = Protocol
