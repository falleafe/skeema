package mybase

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// CommandLine stores state relating to executing an application.
type CommandLine struct {
	InvokedAs    string            // How the bin was invoked; e.g. os.Args[0]
	Command      *Command          // Which command (or subcommand) is being executed
	OptionValues map[string]string // Option values parsed from the command-line
	ArgValues    []string          // Positional arg values (does not include InvokedAs or Command.Name)
}

// OptionValue returns the value for the requested option if it was specified
// on the command-line. This is satisfies the OptionValuer interface, allowing
// Config to use the command-line as the highest-priority option provider.
func (cli *CommandLine) OptionValue(optionName string) (string, bool) {
	value, ok := cli.OptionValues[optionName]
	return value, ok
}

func (cli *CommandLine) parseLongArg(arg string, args *[]string, longOptionIndex map[string]*Option) error {
	key, value, hasValue, loose := NormalizeOptionToken(arg)
	opt, found := longOptionIndex[key]
	if !found {
		if loose {
			return nil
		}
		return OptionNotDefinedError{key, "CLI"}
	}

	// Use returned hasValue boolean instead of comparing value to "", since "" may
	// be set explicitly (--some-opt='') or implicitly (--skip-some-bool-opt) and
	// both of those cases treat hasValue=true
	if !hasValue {
		if opt.RequireValue {
			// Value required: slurp next arg to allow format "--foo bar" in addition to "--foo=bar"
			if len(*args) == 0 || strings.HasPrefix((*args)[0], "-") {
				return OptionMissingValueError{opt.Name, "CLI"}
			}
			value = (*args)[0]
			*args = (*args)[1:]
		} else if opt.Type == OptionTypeBool {
			// Boolean without value is treated as true
			value = "1"
		}
	}

	cli.OptionValues[opt.Name] = value
	return nil
}

func (cli *CommandLine) parseShortArgs(arg string, args *[]string, shortOptionIndex map[rune]*Option) error {
	runeList := []rune(arg)
	var done bool
	for len(runeList) > 0 && !done {
		short := runeList[0]
		runeList = runeList[1:]
		var value string
		opt, found := shortOptionIndex[short]
		if !found {
			return OptionNotDefinedError{string(short), "CLI"}
		}

		// Consume value. Depending on the option, value may be supplied as chars immediately following
		// this one, or after a space as next arg on CLI.
		if len(runeList) > 0 && opt.Type != OptionTypeBool { // "-xvalue", only supported for non-bools
			value = string(runeList)
			done = true
		} else if opt.RequireValue { // "-x value", only supported if opt requires a value
			if len(*args) > 0 && !strings.HasPrefix((*args)[0], "-") {
				value = (*args)[0]
				*args = (*args)[1:]
			} else {
				return OptionMissingValueError{opt.Name, "CLI"}
			}
		} else { // "-xyz", parse x as a valueless option and loop again to parse y (and possibly z) as separate shorthand options
			if opt.Type == OptionTypeBool {
				value = "1" // booleans handle lack of value as being true, whereas other types keep it as empty string
			}
		}

		cli.OptionValues[opt.Name] = value
	}
	return nil
}

// ParseCLI parses the command-line to generate a CommandLine, which
// stores which (sub)command was used, named option values, and positional arg
// values. The CommandLine will then be wrapped in a Config for returning.
//
// The supplied cmd should typically be a root Command (one with nil
// ParentCommand), but this is not a requirement.
//
// The supplied args should match format of os.Args; i.e. args[0]
// should contain the program name.
func ParseCLI(cmd *Command, args []string) (*Config, error) {
	if len(args) == 0 {
		return nil, errors.New("ParseCLI: No command-line supplied")
	}

	cli := &CommandLine{
		Command:      cmd,
		InvokedAs:    args[0],
		OptionValues: make(map[string]string),
		ArgValues:    make([]string, 0),
	}
	args = args[1:]

	// Index options by shorthand
	longOptionIndex := cmd.Options()
	shortOptionIndex := make(map[rune]*Option, len(longOptionIndex))
	for name, opt := range longOptionIndex {
		if opt.Shorthand != 0 {
			shortOptionIndex[opt.Shorthand] = longOptionIndex[name]
		}
	}

	var noMoreOptions bool

	// Iterate over the cli args and process each in turn
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch {
		// option terminator
		case arg == "--":
			noMoreOptions = true

		// long option
		case len(arg) > 2 && arg[0:2] == "--" && !noMoreOptions:
			if err := cli.parseLongArg(arg[2:], &args, longOptionIndex); err != nil {
				return nil, err
			}

		// short option(s) -- multiple bools may be combined into one
		case len(arg) > 1 && arg[0] == '-' && !noMoreOptions:
			if err := cli.parseShortArgs(arg[1:], &args, shortOptionIndex); err != nil {
				return nil, err
			}

		// first positional arg is command name if the current command is a command suite
		case len(cli.Command.SubCommands) > 0:
			command, validCommand := cli.Command.SubCommands[arg]
			if !validCommand {
				return nil, fmt.Errorf("Unknown command \"%s\"", arg)
			}
			cli.Command = command

			// Add the options of the new command into our maps. Any name conflicts
			// intentionally override parent versions.
			for name, opt := range command.options {
				longOptionIndex[name] = command.options[name]
				if opt.Shorthand != 0 {
					shortOptionIndex[opt.Shorthand] = command.options[name]
				}
			}

		// mistakenly supplying help or version as positional arg to a non-command-suite
		case len(cli.ArgValues) == 0 && (arg == "help" || arg == "version"):
			if err := cli.parseLongArg(arg, &args, longOptionIndex); err != nil {
				return nil, err
			}

		// superfluous positional arg
		case len(cli.ArgValues) >= len(cli.Command.args):
			return nil, fmt.Errorf("Extra command-line arg \"%s\" supplied; command %s takes a max of %d args", arg, cli.Command.Name, len(cli.Command.args))

		// positional arg
		default:
			cli.ArgValues = append(cli.ArgValues, arg)
		}
	}

	if len(cli.ArgValues) < cli.Command.minArgs() {
		return nil, fmt.Errorf("Too few positional args supplied on command line; command %s requires at least %d args", cli.Command.Name, cli.Command.minArgs())
	}

	cfg := NewConfig(cli)

	// Handle --help if supplied as an option instead of as a subcommand
	// (Note that format "command help [<subcommand>]" is already parsed properly into help command)
	if forCommandName, helpWanted := cli.OptionValues["help"]; helpWanted {
		// command --help displays help for command
		// vs
		// command --help <subcommand> displays help for subcommand
		cli.ArgValues = []string{forCommandName}
		helpHandler(cfg)
		os.Exit(0)
	}

	// Handle --version if supplied as an option instead of as a subcommand
	if cli.OptionValues["version"] == "1" {
		versionHandler(cfg)
		os.Exit(0)
	}

	// If no command supplied on a command suite, redirect to help subcommand
	if len(cli.Command.SubCommands) > 0 {
		cli.Command = cli.Command.SubCommands["help"]
	}

	return cfg, nil
}