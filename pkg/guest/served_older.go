//go:build disco_vm_older_agent

package guest

// servedProtocol, in a build tagged disco_vm_older_agent, is the protocol
// before Protocol: an agent from before a protocol change, which a test
// boots to see a driver move it forward (vz's macOS guests) or refuse it
// (vz's Linux guests). No release is built with the tag.
const servedProtocol = Protocol - 1
