package machine

// libkrun is the Linux default, with no fallback: a host that cannot run it
// is refused by its Check, which names another driver (ADR 26-11-03-530 §11).
const defaultDriver = "libkrun"
