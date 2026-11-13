//go:build disco_vm_older_agent

package guest

// servedProtocol, in a build tagged disco_vm_older_agent, is the protocol
// before Protocol: an agent from before a protocol change, which a test
// boots to see a driver put it behind vz's agent stub (a macOS guest
// installed before it) or refuse it (vz's Linux guests). No release is built with the tag.
const servedProtocol = Protocol - 1
