package src

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kr/fs"
	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sourcegraph/srclib/buildstore"
	"sourcegraph.com/sourcegraph/srclib/config"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/grapher"
	"sourcegraph.com/sourcegraph/srclib/plan"
	"sourcegraph.com/sourcegraph/srclib/unit"
)

func init() {
	c, err := CLI.AddCommand("api",
		"API",
		"",
		&apiCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("describe",
		"display documentation for the def under the cursor",
		"Returns information about the definition referred to by the cursor's current position in a file.",
		&apiDescribeCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type APICmd struct{}

var apiCmd APICmd

func (c *APICmd) Execute(args []string) error { return nil }

type APIDescribeCmd struct {
	File      string `long:"file" required:"yes" value-name:"FILE"`
	StartByte int    `long:"start-byte" required:"yes" value-name:"BYTE"`

	Examples bool `long:"examples" describe:"show examples from Sourcegraph.com"`
}

var apiDescribeCmd APIDescribeCmd

func (c *APIDescribeCmd) Execute(args []string) error {
	repo, err := OpenRepo(filepath.Dir(c.File))
	if err != nil {
		return err
	}

	c.File, err = filepath.Rel(repo.RootDir, c.File)
	if err != nil {
		return err
	}

	if err := os.Chdir(repo.RootDir); err != nil {
		return err
	}

	buildStore, err := buildstore.NewRepositoryStore(repo.RootDir)
	if err != nil {
		return err
	}

	configOpt := config.Options{
		Repo:   string(repo.URI()),
		Subdir: ".",
	}
	toolchainExecOpt := ToolchainExecOpt{ExeMethods: "program"}

	// Config repository if not yet built.
	if _, err := buildStore.Stat(buildStore.CommitPath(repo.CommitID)); os.IsNotExist(err) {
		configCmd := &ConfigCmd{
			Options:          configOpt,
			ToolchainExecOpt: toolchainExecOpt,
		}
		if err := configCmd.Execute(nil); err != nil {
			return err
		}
	}

	// Always re-make.
	//
	// TODO(sqs): optimize this
	makeCmd := &MakeCmd{
		Options:          configOpt,
		ToolchainExecOpt: toolchainExecOpt,
	}
	if err := makeCmd.Execute(nil); err != nil {
		return err
	}

	// TODO(sqs): This whole lookup is totally inefficient. The storage format
	// is not optimized for lookups.

	// Find all source unit definition files.
	var unitFiles []string
	unitSuffix := buildstore.DataTypeSuffix(unit.SourceUnit{})
	w := fs.WalkFS(buildStore.CommitPath(repo.CommitID), buildStore)
	for w.Step() {
		if strings.HasSuffix(w.Path(), unitSuffix) {
			unitFiles = append(unitFiles, w.Path())
		}
	}

	// Find which source units the file belongs to.
	var units []*unit.SourceUnit
	for _, unitFile := range unitFiles {
		var u *unit.SourceUnit
		f, err := buildStore.Open(unitFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&u); err != nil {
			return fmt.Errorf("%s: %s", unitFile, err)
		}
		for _, f2 := range u.Files {
			if f2 == c.File {
				units = append(units, u)
				break
			}
		}
	}

	if GlobalOpt.Verbose {
		if len(units) > 0 {
			ids := make([]string, len(units))
			for i, u := range units {
				ids[i] = string(u.ID())
			}
			log.Printf("Position %s:%d is in %d source units %v.", c.File, c.StartByte, len(units), ids)
		} else {
			log.Printf("Position %s:%d is not in any source units.", c.File, c.StartByte)
		}
	}

	// Find the ref(s) at the character position.
	var ref *graph.Ref
OuterLoop:
	for _, u := range units {
		var g grapher.Output
		graphFile := buildStore.FilePath(repo.CommitID, plan.SourceUnitDataFilename("graph", u))
		f, err := buildStore.Open(graphFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&g); err != nil {
			return fmt.Errorf("%s: %s", graphFile, err)
		}
		for _, ref2 := range g.Refs {
			if c.File == ref2.File && c.StartByte >= ref2.Start && c.StartByte <= ref2.End {
				ref = ref2
				if ref.DefUnit == "" {
					ref.DefUnit = u.Name
				}
				if ref.DefUnitType == "" {
					ref.DefUnitType = u.Type
				}
				break OuterLoop
			}
		}
	}

	if ref == nil {
		if GlobalOpt.Verbose {
			log.Printf("No ref found at %s:%d.", c.File, c.StartByte)
		}
		fmt.Println(`{}`)
		return nil
	}

	if ref.DefRepo == "" {
		ref.DefRepo = repo.URI()
	}

	var resp struct {
		Def      *sourcegraph.Def
		Examples []*sourcegraph.Example
	}

	// Now find the def for this ref.
	defInCurrentRepo := ref.DefRepo == repo.URI()
	if defInCurrentRepo {
		// Def is in the current repo.
		var g grapher.Output
		graphFile := buildStore.FilePath(repo.CommitID, plan.SourceUnitDataFilename("graph", &unit.SourceUnit{Name: ref.DefUnit, Type: ref.DefUnitType}))
		f, err := buildStore.Open(graphFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&g); err != nil {
			return fmt.Errorf("%s: %s", graphFile, err)
		}
		for _, def2 := range g.Defs {
			if def2.Path == ref.DefPath {
				resp.Def = &sourcegraph.Def{Def: *def2}
				break
			}
		}
		if resp.Def != nil {
			for _, doc := range g.Docs {
				if doc.Path == ref.DefPath {
					resp.Def.DocHTML = doc.Data
				}
			}
		}
		if resp.Def == nil && GlobalOpt.Verbose {
			log.Printf("No definition found with path %q in unit %q type %q.", ref.DefPath, ref.DefUnit, ref.DefUnitType)
		}
	}

	spec := sourcegraph.DefSpec{
		Repo:     string(ref.DefRepo),
		UnitType: ref.DefUnitType,
		Unit:     ref.DefUnit,
		Path:     string(ref.DefPath),
	}

	var wg sync.WaitGroup

	if resp.Def == nil {
		// Def is not in the current repo. Try looking it up using the
		// Sourcegraph API.
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			resp.Def, _, err = apiclient.Defs.Get(spec, &sourcegraph.DefGetOptions{Doc: true})
			if err != nil && GlobalOpt.Verbose {
				log.Printf("Couldn't fetch definition %v: %s.", spec, err)
			}
		}()
	}

	if fetchExamples := true; fetchExamples {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			resp.Examples, _, err = apiclient.Defs.ListExamples(spec, &sourcegraph.DefListExamplesOptions{
				Formatted:   true,
				ListOptions: sourcegraph.ListOptions{PerPage: 4},
			})
			if err != nil && GlobalOpt.Verbose {
				log.Printf("Couldn't fetch examples for %v: %s.", spec, err)
			}
		}()
	}

	wg.Wait()

	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return err
	}
	return nil
}
