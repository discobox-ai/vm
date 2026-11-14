package guest

// What `disco-vm password-is-name USER` prints in a Windows guest: the
// account still has its own name for a password, or has another.
const (
	PasswordName  = "name"
	PasswordOther = "other"
)
