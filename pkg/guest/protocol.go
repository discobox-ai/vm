package guest

// ServedProtocol is the protocol this build's agent reports in its info:
// Protocol, except in a build tagged disco_vm_older_agent. `disco-vm guest
// --protocol` prints it, so a host can see that a binary it put in a guest is
// an agent of its protocol before that binary takes the running one's place.
func ServedProtocol() int { return servedProtocol }
