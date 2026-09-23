module github.com/discobox-ai/vm

go 1.26.1

require (
	github.com/Code-Hex/vz/v3 v3.7.1
	github.com/Microsoft/go-winio v0.6.2
	github.com/creack/pty v1.1.24
	github.com/spf13/cobra v1.10.2
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
)

require (
	github.com/Code-Hex/go-infinity-channel v1.0.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/mod v0.22.0 // indirect
)

// Code-Hex/vz closes the descriptor a VZVirtioSocketConnection owns, so the
// framework's own close lands on whatever reused the number. discobox's fork
// duplicates it instead, and adds WithMacGuestProvisioning (macOS 27), which
// the vz driver's unattended install needs. Keep the pin in step with
// discobox's server/go.mod.
replace github.com/Code-Hex/vz/v3 => github.com/discobox-ai/vz/v3 v3.7.2-0.20260922054351-c8cee82c6395
