package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/discobox-ai/vm/pkg/machine"
)

// driverConfigUsage is --driver-config's help.
const driverConfigUsage = "the driver's whole configuration, as JSON or @FILE, in place of its flags and environment variables (see --help=DRIVER)"

// driverFlag is the flag of one field of one driver's configuration:
// --DRIVER-FIELD, so no two drivers' fields, and no command's flag, share a
// name.
type driverFlag struct {
	driver string
	field  machine.ConfigField
	flag   *pflag.Flag
}

// driverFlagName is the flag of a driver's field: its key in kebab case,
// after the driver's name.
func driverFlagName(driver, key string) string {
	var b strings.Builder
	b.WriteString(driver)
	b.WriteByte('-')
	for _, r := range key {
		if unicode.IsUpper(r) {
			b.WriteByte('-')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// addDriverFlags adds a hidden flag for every field of every driver in the
// build to flags, which --help=DRIVER lists.
func (g *globals) addDriverFlags(flags *pflag.FlagSet) {
	for _, name := range machine.Names() {
		def, err := machine.DefinitionOf(name)
		if err != nil {
			continue
		}
		// A retired field keeps its environment variable and has no flag
		// (ADR 26-11-11-215 §3).
		for _, field := range def.Current() {
			flagName := driverFlagName(name, field.Key)
			flags.String(flagName, "", field.Description)
			f := flags.Lookup(flagName)
			if field.Type == machine.ConfigBoolean {
				f.NoOptDefVal = "true"
			}
			f.Hidden = true
			g.driverFlags = append(g.driverFlags, driverFlag{driver: name, field: field, flag: f})
		}
	}
}

// driverConfig is the named driver's configuration (ADR 26-11-11-215 §4):
// --driver-config, whole, with no environment variable read; or each
// field's flag, else its environment variable. A flag of another driver's
// field is refused, as is one beside --driver-config. The driver validates
// what this returns (machine.New).
func (g *globals) driverConfig(name string) (json.RawMessage, error) {
	var set []driverFlag
	for _, f := range g.driverFlags {
		if !f.flag.Changed {
			continue
		}
		if f.driver != name {
			return nil, fmt.Errorf("--%s configures the %s driver, and the driver is %s", f.flag.Name, f.driver, name)
		}
		set = append(set, f)
	}
	if g.config != "" {
		if len(set) > 0 {
			return nil, fmt.Errorf("--driver-config cannot be combined with --%s", set[0].flag.Name)
		}
		config, err := readConfigArg(g.config)
		if err != nil {
			return nil, err
		}
		return withExecutable(name, config)
	}
	values := machine.ConfigFromEnv(name)
	for _, f := range set {
		values[f.field.Key] = f.flag.Value.String()
	}
	// A retired field's variable gives way to its replacement, given by its
	// flag or its own variable: only a configuration that names both is
	// refused (machine.Normalize), and here nobody wrote both.
	if def, err := machine.DefinitionOf(name); err == nil {
		for _, f := range def.ConfigFields {
			if _, current := values[f.ReplacedBy]; f.Retired && current {
				delete(values, f.Key)
			}
		}
	}
	config, err := machine.ConfigFromValues(name, values)
	if err != nil {
		return nil, err
	}
	return withExecutable(name, config)
}

// withExecutable sets each field of the named driver's that defaults to the
// executable (ConfigField.DefaultExecutable) and that config leaves out to
// this disco-vm, which the driver then hands its guests: vz's macOS agent.
func withExecutable(name string, config json.RawMessage) (json.RawMessage, error) {
	def, err := machine.DefinitionOf(name)
	if err != nil {
		return config, nil //nolint:nilerr // an unknown driver is machine.New's to refuse
	}
	var fields map[string]any
	if len(bytes.TrimSpace(config)) > 0 {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, fmt.Errorf("%s: configuration: not a JSON object", name)
		}
	}
	if fields == nil {
		// No configuration, or JSON's null, which decodes to no map.
		fields = map[string]any{}
	}
	changed := false
	for _, f := range def.Current() {
		if _, ok := fields[f.Key]; ok || !f.DefaultExecutable {
			continue
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("%s: configuration: %s: this executable: %w", name, f.Key, err)
		}
		fields[f.Key], changed = exe, true
	}
	if !changed {
		return config, nil
	}
	return json.Marshal(fields)
}

// configInEffect is the named driver's configuration as it runs with it:
// normalized, with each absent field's default.
func (g *globals) configInEffect(name string) (json.RawMessage, error) {
	config, err := g.driverConfig(name)
	if err != nil {
		return nil, err
	}
	if config, err = machine.Normalize(name, config); err != nil {
		return nil, err
	}
	def, err := machine.DefinitionOf(name)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, err
		}
	}
	for _, f := range def.Current() {
		if _, ok := fields[f.Key]; !ok && f.Default != "" {
			if fields[f.Key], err = f.Parse(f.Default); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(fields)
}

// maxConfigFile is the most --driver-config @FILE reads.
const maxConfigFile = 1 << 20

// readConfigArg is --driver-config's JSON: the argument itself, or the file
// @PATH names, at most maxConfigFile. Only its parsed JSON goes on, and no
// error quotes it, nor the file.
func readConfigArg(arg string) (json.RawMessage, error) {
	data := []byte(arg)
	if path, ok := strings.CutPrefix(arg, "@"); ok {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("--driver-config: %w", err)
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, maxConfigFile+1)); err != nil {
			return nil, fmt.Errorf("--driver-config: %w", err)
		}
		if len(data) > maxConfigFile {
			return nil, fmt.Errorf("--driver-config: %s is over %d bytes", path, maxConfigFile)
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		if se, ok := errors.AsType[*json.SyntaxError](err); ok {
			return nil, fmt.Errorf("--driver-config: not JSON (at byte %d)", se.Offset)
		}
		return nil, errors.New("--driver-config: not JSON")
	}
	return compact.Bytes(), nil
}

// warnEnvIgnored names, on stderr and never with its value, each variable
// of the driver's fields that is set and says otherwise than a whole
// --driver-config, which reads none of them.
func (g *globals) warnEnvIgnored(name string, config json.RawMessage) {
	if g.config == "" {
		return
	}
	for _, env := range machine.ConfigEnvDisagrees(name, config) {
		fmt.Fprintf(os.Stderr, "disco-vm: warning: $%s is set and not read: --driver-config is the %s driver's whole configuration\n", env, name)
	}
}

// helpFlag is --help, and --help=DRIVER, which shows the driver's
// configuration in place of the command's help.
type helpFlag struct {
	driver string
	set    bool
}

func (f *helpFlag) Set(value string) error {
	if b, err := strconv.ParseBool(value); err == nil {
		f.set, f.driver = b, ""
		return nil
	}
	f.set, f.driver = true, strings.TrimSpace(value)
	return nil
}

func (f *helpFlag) String() string   { return strconv.FormatBool(f.set) }
func (f *helpFlag) Type() string     { return "bool" }
func (f *helpFlag) IsBoolFlag() bool { return true }

// installHelp makes --help on every command take an optional driver, and
// shows that driver's configuration when it is given.
func (g *globals) installHelp(root *cobra.Command) {
	help := &helpFlag{}
	root.PersistentFlags().VarP(help, "help", "h", "help for a command; --help=DRIVER for the driver's configuration")
	root.PersistentFlags().Lookup("help").NoOptDefVal = "true"
	commandHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if help.driver == "" {
			commandHelp(cmd, args)
			return
		}
		if err := writeDriverHelp(cmd.OutOrStdout(), help.driver); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "disco-vm:", err)
		}
	})
}

// writeDriverHelp shows a driver's configuration: its base, or that it
// takes none, its key's variables, and each current field's flag, its key in
// --driver-config, its environment variable, and its default.
func writeDriverHelp(w io.Writer, driver string) error {
	def, err := machine.DefinitionOf(driver)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "The %s driver", driver)
	if def.Description != "" {
		fmt.Fprintf(w, ": %s", def.Description)
	}
	fmt.Fprintf(w, "\n\nUsage:\n  disco-vm --driver %s [driver flags] COMMAND\n", driver)
	if def.TakesBase() {
		fmt.Fprintf(w, "\nBase ($%s, what a fresh VM is made from):\n", machine.BaseEnv)
		fmt.Fprintf(w, "        %s\n", def.Base.Description)
		if details := fieldDetails(*def.Base, ""); details != "" {
			fmt.Fprintf(w, "        (%s)\n", details)
		}
	} else {
		fmt.Fprintf(w, "\nThe %s driver takes no base: a fresh VM is refused.\n", driver)
	}
	// A secret is listed by where it is read from, never by a value.
	for _, secret := range def.Secrets {
		fmt.Fprintf(w, "\nKey (secret; $%s):\n", strings.Join(secret.Env, " or $"))
		fmt.Fprintf(w, "        %s\n", secret.Description)
	}
	fields := def.Current()
	if len(fields) == 0 {
		fmt.Fprintf(w, "\nThe %s driver takes no configuration.\n", driver)
		return nil
	}
	fmt.Fprintln(w, "\nDriver Flags:")
	for _, field := range fields {
		flag := "--" + driverFlagName(driver, field.Key)
		if field.Type != machine.ConfigBoolean {
			flag += " " + field.Type
		}
		fmt.Fprintf(w, "  %s\n", flag)
		fmt.Fprintf(w, "        %s\n", field.Description)
		fmt.Fprintf(w, "        (%s)\n", fieldDetails(field, "key: "+field.Key))
	}
	fmt.Fprintf(w, `
A flag takes the place of its environment variable. Or give the whole
configuration as a JSON object of the keys above, in place of both; then no
environment variable is read:
  disco-vm --driver %s --driver-config '{"%s": ...}' COMMAND
  disco-vm --driver %s --driver-config @%s.json COMMAND
A privileged field names a host resource, or where the driver's key or a
guest's data is sent: let no one set it who could not be handed both.
`, driver, fields[0].Key, driver, driver)
	return nil
}

// fieldDetails is a field's key, variable, default, and marks, for help.
func fieldDetails(field machine.ConfigField, key string) string {
	var details []string
	if key != "" {
		details = append(details, key)
	}
	if field.Env != "" && key != "" {
		details = append(details, "env: $"+field.Env)
	}
	if field.Default != "" {
		details = append(details, "default: "+field.Default)
	} else if field.DefaultExecutable {
		details = append(details, "default: this disco-vm")
	} else if field.Placeholder != "" {
		details = append(details, "e.g. "+field.Placeholder)
	}
	if field.Required {
		details = append(details, "required")
	}
	if field.Advanced {
		details = append(details, "advanced")
	}
	if field.Privileged {
		details = append(details, "privileged")
	}
	return strings.Join(details, "; ")
}
