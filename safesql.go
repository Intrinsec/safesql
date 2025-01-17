// Command safesql is a tool for performing static analysis on programs to
// ensure that SQL injection attacks are not possible. It does this by ensuring
// package database/sql is only used with compile-time constant queries.
package main

import (
	"flag"
	"fmt"
	"go/build"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"sort"

	"path/filepath"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/pointer"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const IgnoreComment = "//nolint:safesql"

type sqlPackage struct {
	packageName string
	paramNames  []string
	enable      bool
}

var sqlPackages = []sqlPackage{
	{
		packageName: "database/sql",
		paramNames:  []string{"query"},
	},
	{
		packageName: "github.com/jinzhu/gorm",
		paramNames:  []string{"sql", "query"},
	},
	{
		packageName: "github.com/jmoiron/sqlx",
		paramNames:  []string{"query"},
	},
	{
		packageName: "github.com/jackc/pgx/v4",
		paramNames:  []string{"sql", "query"},
	},
}

var ignoredFiles = []string{
	"github.com/jackc/pgx/v4/pgxpool/conn.go",
}

func main() {
	var verbose, quiet bool
	flag.BoolVar(&verbose, "v", false, "Verbose mode")
	flag.BoolVar(&quiet, "q", false, "Only print on failure")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-q] [-v] package1 [package2 ...]\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()
	pkgs := flag.Args()
	if len(pkgs) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	c := loader.Config{
		FindPackage: FindPackage,
	}
	for _, pkg := range pkgs {
		c.Import(pkg)
	}
	p, err := c.Load()

	if err != nil {
		fmt.Printf("error loading packages %v: %v\n", pkgs, err)
		os.Exit(2)
	}

	imports := getImports(p)
	existOne := false
	for i := range sqlPackages {
		if _, exist := imports[sqlPackages[i].packageName]; exist {
			if verbose {
				fmt.Printf("Enabling support for %s\n", sqlPackages[i].packageName)
			}
			sqlPackages[i].enable = true
			existOne = true
		}
	}
	if !existOne {
		fmt.Printf("No packages in %v include a supported database driver", pkgs)
		os.Exit(2)
	}

	s := ssautil.CreateProgram(p, 0)
	s.Build()

	qms := make([]*QueryMethod, 0)

	for i := range sqlPackages {
		if sqlPackages[i].enable {
			qms = append(qms, FindQueryMethods(sqlPackages[i], p.Package(sqlPackages[i].packageName).Pkg, s)...)
		}
	}

	if verbose {
		fmt.Println("database driver functions that accept queries:")
		for _, m := range qms {
			fmt.Printf("- %s (param %d)\n", m.Func, m.Param)
		}
		fmt.Println()
	}

	mains := FindMains(p, s)
	if len(mains) == 0 {
		fmt.Println("Did not find any commands (i.e., main functions).")
		os.Exit(2)
	}

	res, err := pointer.Analyze(&pointer.Config{
		Mains:          mains,
		BuildCallGraph: true,
	})
	if err != nil {
		fmt.Printf("error performing pointer analysis: %v\n", err)
		os.Exit(2)
	}

	bad := FindNonConstCalls(res.CallGraph, qms)

	if len(bad) == 0 {
		if !quiet {
			fmt.Println(`You're safe from SQL injection! Yay \o/`)
		}
		return
	}

	if verbose {
		fmt.Printf("Found %d potentially unsafe SQL statements:\n", len(bad))
	}

	potentialBadStatements := []token.Position{}
	for _, ci := range bad {
		potentialBadStatements = append(potentialBadStatements, p.Fset.Position(ci.Pos()))
	}

	issues, err := CheckIssues(potentialBadStatements)
	if err != nil {
		fmt.Printf("error when checking for ignore comments: %v\n", err)
		os.Exit(2)
	}

	if verbose {
		fmt.Println("Please ensure that all SQL queries you use are compile-time constants.")
		fmt.Println("You should always use parameterized queries or prepared statements")
		fmt.Println("instead of building queries from strings.")
	}

	hasNonIgnoredUnsafeStatement := false

	for _, issue := range issues {
		if issue.ignored {
			fmt.Printf("- %s is potentially unsafe but file is ignored or statement ignored by comment\n", issue.statement)
		} else {
			fmt.Printf("- %s\n", issue.statement)
			hasNonIgnoredUnsafeStatement = true
		}
	}

	if hasNonIgnoredUnsafeStatement {
		os.Exit(1)
	}
}

// QueryMethod represents a method on a type which has a string parameter named
// "query".
type QueryMethod struct {
	Func     *types.Func
	SSA      *ssa.Function
	ArgCount int
	Param    int
}

type Issue struct {
	statement token.Position
	ignored   bool
}

// CheckIssues checks lines to see if the line before or the current line has an ignore comment and marks those
// statements that have the ignore comment on the current line or the line before
func CheckIssues(lines []token.Position) ([]Issue, error) {
	files := make(map[string][]token.Position)

	for _, line := range lines {
		files[line.Filename] = append(files[line.Filename], line)
	}

	issues := []Issue{}

	for file, linesInFile := range files {

		fileIsIgnored := false

		// check for ignored files
		for _, ignoredFile := range ignoredFiles {
			if strings.HasSuffix(file, ignoredFile) {
				fileIsIgnored = true
				break
			}
		}

		// ensure we have the lines in ascending order
		sort.Slice(linesInFile, func(i, j int) bool { return linesInFile[i].Line < linesInFile[j].Line })

		data, err := ioutil.ReadFile(file)
		if err != nil {
			return nil, err
		}
		fileLines := strings.Split(string(data), "\n")

		for _, line := range linesInFile {
			// check the line before the problematic statement first
			potentialCommentLine := line.Line - 2

			// check only if the previous line is strictly a line that begins with
			// the ignore comment
			if 0 <= potentialCommentLine && BeginsWithComment(fileLines[potentialCommentLine]) {
				issues = append(issues, Issue{statement: line, ignored: true})
				continue
			}

			isIgnored := HasIgnoreComment(fileLines[line.Line-1]) || fileIsIgnored
			issues = append(issues, Issue{statement: line, ignored: isIgnored})
		}
	}

	return issues, nil
}

func BeginsWithComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), IgnoreComment)
}

func HasIgnoreComment(line string) bool {
	return strings.HasSuffix(strings.TrimSpace(line), IgnoreComment)
}

// FindQueryMethods locates all methods in the given package (assumed to be
// package database/sql) with a string parameter named "query".
func FindQueryMethods(sqlPackages sqlPackage, sql *types.Package, ssa *ssa.Program) []*QueryMethod {
	methods := make([]*QueryMethod, 0)
	scope := sql.Scope()
	for _, name := range scope.Names() {
		o := scope.Lookup(name)
		if !o.Exported() {
			continue
		}
		if _, ok := o.(*types.TypeName); !ok {
			continue
		}
		n := o.Type().(*types.Named)
		for i := 0; i < n.NumMethods(); i++ {
			m := n.Method(i)
			if !m.Exported() {
				continue
			}
			s := m.Type().(*types.Signature)
			if num, ok := FuncHasQuery(sqlPackages, s); ok {
				methods = append(methods, &QueryMethod{
					Func:     m,
					SSA:      ssa.FuncValue(m),
					ArgCount: s.Params().Len(),
					Param:    num,
				})
			}
		}
	}
	return methods
}

// FuncHasQuery returns the offset of the string parameter named "query", or
// none if no such parameter exists.
func FuncHasQuery(sqlPackages sqlPackage, s *types.Signature) (offset int, ok bool) {
	params := s.Params()
	for i := 0; i < params.Len(); i++ {
		v := params.At(i)
		for _, paramName := range sqlPackages.paramNames {
			if v.Name() == paramName {
				return i, true
			}
		}
	}
	return 0, false
}

// FindMains returns the set of all packages loaded into the given
// loader.Program which contain main functions
func FindMains(p *loader.Program, s *ssa.Program) []*ssa.Package {
	ips := p.InitialPackages()
	mains := make([]*ssa.Package, 0, len(ips))
	for _, info := range ips {
		ssaPkg := s.Package(info.Pkg)
		if ssaPkg.Func("main") != nil {
			mains = append(mains, ssaPkg)
		}
	}
	return mains
}

func getImports(p *loader.Program) map[string]interface{} {
	pkgs := make(map[string]interface{})
	for _, pkg := range p.AllPackages {
		if pkg.Importable {
			pkgs[pkg.Pkg.Path()] = nil
		}
	}
	return pkgs
}

// FindNonConstCalls returns the set of callsites of the given set of methods
// for which the "query" parameter is not a compile-time constant.
func FindNonConstCalls(cg *callgraph.Graph, qms []*QueryMethod) []ssa.CallInstruction {
	cg.DeleteSyntheticNodes()

	// package database/sql has a couple helper functions which are thin
	// wrappers around other sensitive functions. Instead of handling the
	// general case by tracing down callsites of wrapper functions
	// recursively, let's just whitelist the functions we're already
	// tracking, since it happens to be good enough for our use case.
	okFuncs := make(map[*ssa.Function]struct{}, len(qms))
	for _, m := range qms {
		okFuncs[m.SSA] = struct{}{}
	}

	bad := make([]ssa.CallInstruction, 0)
	for _, m := range qms {
		node := cg.CreateNode(m.SSA)
		for _, edge := range node.In {
			if _, ok := okFuncs[edge.Site.Parent()]; ok {
				continue
			}

			isInternalSQLPkg := false
			for _, pkg := range sqlPackages {
				if pkg.packageName == edge.Caller.Func.Pkg.Pkg.Path() {
					isInternalSQLPkg = true
					break
				}
			}
			if isInternalSQLPkg {
				continue
			}

			cc := edge.Site.Common()
			args := cc.Args
			// The first parameter is occasionally the receiver.
			if len(args) == m.ArgCount+1 {
				args = args[1:]
			} else if len(args) != m.ArgCount {
				panic("arg count mismatch")
			}
			v := args[m.Param]

			if _, ok := v.(*ssa.Const); !ok {
				if inter, ok := v.(*ssa.MakeInterface); ok && types.IsInterface(v.(*ssa.MakeInterface).Type()) {
					if inter.X.Referrers() == nil || inter.X.Type() != types.Typ[types.String] {
						continue
					}
				}

				bad = append(bad, edge.Site)
			}
		}
	}

	return bad
}

// Deal with GO15VENDOREXPERIMENT
func FindPackage(ctxt *build.Context, path, dir string, mode build.ImportMode) (*build.Package, error) {
	if !useVendor {
		return ctxt.Import(path, dir, mode)
	}

	// First, walk up the filesystem from dir looking for vendor directories
	var vendorDir string
	for tmp := dir; vendorDir == "" && tmp != "/"; tmp = filepath.Dir(tmp) {
		dname := filepath.Join(tmp, "vendor", filepath.FromSlash(path))
		fd, err := os.Open(dname)
		if err != nil {
			continue
		}
		// Directories are only valid if they contain at least one file
		// with suffix ".go" (this also ensures that the file descriptor
		// we have is in fact a directory)
		names, err := fd.Readdirnames(-1)
		if err != nil {
			continue
		}
		for _, name := range names {
			if strings.HasSuffix(name, ".go") {
				vendorDir = filepath.ToSlash(dname)
				break
			}
		}
	}

	if vendorDir != "" {
		pkg, err := ctxt.ImportDir(vendorDir, mode)
		if err != nil {
			return nil, err
		}
		// Go tries to derive a valid import path for the package, but
		// it's wrong (it includes "/vendor/"). Overwrite it here.
		pkg.ImportPath = path
		return pkg, nil
	}

	return ctxt.Import(path, dir, mode)
}
