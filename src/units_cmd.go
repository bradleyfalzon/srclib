package src

import (
	"fmt"
	"log"
	"path/filepath"

	"strings"

	"sourcegraph.com/sourcegraph/srclib/config"
	"sourcegraph.com/sourcegraph/srclib/scan"
	"sourcegraph.com/sourcegraph/srclib/toolchain"
)

func init() {
	c, err := CLI.AddCommand("units",
		"lists source units",
		`Lists source units in the repository or directory tree rooted at DIR (or the current directory if DIR is not specified).`,
		&unitsCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	SetRepoOptDefaults(c)
}

// scanUnitsIntoConfig uses cfg to scan for source units. It modifies
// cfg.SourceUnits, merging the scanned source units with those already present
// in cfg.
func scanUnitsIntoConfig(cfg *config.Repository, configOpt config.Options, execOpt ToolchainExecOpt) error {
	scanners := make([]toolchain.Tool, len(cfg.Scanners))
	for i, scannerRef := range cfg.Scanners {
		scanner, err := toolchain.OpenTool(scannerRef.Toolchain, scannerRef.Subcmd, execOpt.ToolchainMode())
		if err != nil {
			return err
		}
		scanners[i] = scanner
	}

	units, err := scan.ScanMulti(scanners, scan.Options{configOpt}, cfg.Config)
	if err != nil {
		return err
	}

	// Copy the repo/tree config to each source unit.
	for _, u := range units {
		// TODO(sqs): merge the config, don't just clobber the config (in case
		// the scanner set any in the units it produces)
		if cfg.Config != nil {
			u.Config = cfg.Config
		} else {
			u.Config = map[string]interface{}{}
		}
	}

	// TODO(sqs): merge the Srcfile's source units with the ones we scanned;
	// don't just clobber them.

	for _, u := range units {
		unitDir := u.Dir
		if unitDir == "" && len(u.Files) > 0 {
			// in case the unit doesn't specify a Dir, obtain it from the first file
			unitDir = filepath.Dir(u.Files[0])
		}

		// heed SkipDirs
		if pathHasAnyPrefix(unitDir, cfg.SkipDirs) {
			continue
		}

		cfg.SourceUnits = append(cfg.SourceUnits, u)
	}

	return nil
}

type UnitsCmd struct {
	config.Options

	ToolchainExecOpt `group:"execution"`

	Output struct {
		Output string `short:"o" long:"output" description:"output format" default:"text" value-name:"text|json"`
	} `group:"output"`

	Args struct {
		Dir Directory `name:"DIR" default:"." description:"root directory of tree to list units in"`
	} `positional-args:"yes"`
}

var unitsCmd UnitsCmd

func (c *UnitsCmd) Execute(args []string) error {
	if c.Args.Dir == "" {
		c.Args.Dir = "."
	}

	cfg, err := getInitialConfig(c.Options, c.Args.Dir)
	if err != nil {
		return err
	}

	if err := scanUnitsIntoConfig(cfg, c.Options, c.ToolchainExecOpt); err != nil {
		return err
	}

	if c.Output.Output == "json" {
		PrintJSON(cfg.SourceUnits, "")
	} else {
		for _, u := range cfg.SourceUnits {
			fmt.Printf("%-50s  %s\n", u.Name, u.Type)
		}
	}

	return nil
}

func pathHasAnyPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if pathHasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func pathHasPrefix(path, prefix string) bool {
	path = filepath.Clean(path)
	prefix = filepath.Clean(prefix)
	return prefix == "." || path == prefix || strings.HasPrefix(path, prefix+"/")
}
