// go:build ignore
//go:build ignore
// +build ignore

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"
	"text/template"

	"cuelang.org/go/cue/cuecontext"
	cueformat "cuelang.org/go/cue/format"
	"cuelang.org/go/pkg/encoding/yaml"
	"github.com/deepmap/oapi-codegen/pkg/codegen"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/grafana/cuetsy"
	"github.com/grafana/grafana/pkg/cuectx"
	"github.com/grafana/thema"
	"github.com/grafana/thema/encoding/openapi"
	"golang.org/x/tools/imports"
)

var lib = thema.NewLibrary(cuecontext.New())

const sep = string(filepath.Separator)

// Generate Go and Typescript implementations for all coremodels, and populate the
// coremodel static registry.
func main() {
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "coremodel code generator does not currently accept any arguments\n, got %q", os.Args)
		os.Exit(1)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not get working directory: %s", err)
		os.Exit(1)
	}

	// TODO this binds us to only having coremodels in a single directory. If we need more, compgen is the way
	grootp := strings.Split(cwd, sep)
	groot := filepath.Join(sep, filepath.Join(grootp[:len(grootp)-3]...))

	cmroot := filepath.Join(groot, "pkg", "coremodel")
	tsroot := filepath.Join(groot, "packages", "grafana-schema", "src", "schema")

	items, err := ioutil.ReadDir(cmroot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not read coremodels parent dir %s: %s\n", cmroot, err)
		os.Exit(1)
	}

	var lins []linsrc
	for _, item := range items {
		if item.IsDir() {
			lin, err := processCoremodelDir(filepath.Join(cmroot, item.Name()))
			if err != nil {
				fmt.Fprintf(os.Stderr, "could not process coremodels dir %s: %s\n", cmroot, err)
				os.Exit(1)
			}

			lin.relpath = filepath.Join(strings.Split(lin.path, sep)[len(grootp)-3:]...)
			lins = append(lins, lin)
		}
	}

	for _, ls := range lins {
		err = generateGo(filepath.Join(cmroot, ls.lin.Name()), ls)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate Go code for %s: %s\n", ls.lin.Name(), err)
			os.Exit(1)
		}
		err = generateTypescript(filepath.Join(tsroot, ls.lin.Name()), ls)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to generate Go code for %s: %s\n", ls.lin.Name(), err)
			os.Exit(1)
		}
	}

	if err = generateRegistry(filepath.Join(groot, "pkg", "framework", "coremodel", "staticregistry", "registry_gen.go"), lins); err != nil {
		fmt.Fprintf(os.Stderr, "failed to generate coremodel registry: %s\n", err)
		os.Exit(1)
	}
}

// Scan the dir and load up its lineage
func processCoremodelDir(path string) (ls linsrc, err error) {
	ls.path = filepath.Join(path, "lineage.cue")
	f, err := os.Open(ls.path)
	if err != nil {
		return ls, fmt.Errorf("could not open lineage file under %s: %w", path, err)
	}

	byt, err := ioutil.ReadAll(f)
	if err != nil {
		return
	}

	fs := fstest.MapFS{
		"lineage.cue": &fstest.MapFile{
			Data: byt,
		},
	}

	_, name := filepath.Split(path)
	ls.lin, err = cuectx.LoadGrafanaInstancesWithThema(filepath.Join("pkg", "coremodel", name), fs, lib)
	if err != nil {
		return
	}
	ls.PkgName, ls.TitleName = ls.lin.Name(), strings.Title(ls.lin.Name())
	return
}

type linsrc struct {
	lin                thema.Lineage
	path               string
	relpath            string
	TitleName, PkgName string
}

// func getCoremodels() map[string]coremodel.Interface {
//
// 	dash, err := dashboard.ProvideCoremodel(lib)
// 	if err != nil {
// 		panic(err)
// 	}
// 	return map[string]coremodel.Interface{
// 		"dashboard": dash,
// 	}
// }

func generateGo(path string, ls linsrc) error {
	lin := ls.lin
	sch := thema.SchemaP(lin, thema.LatestVersion(lin))
	f, err := openapi.GenerateSchema(sch, nil)
	if err != nil {
		return fmt.Errorf("thema openapi generation failed: %w", err)
	}

	b, err := cueformat.Node(f)
	if err != nil {
		return fmt.Errorf("cue format printing failed: %w", err)
	}

	_ = b

	str, err := yaml.Marshal(lib.Context().BuildFile(f))
	if err != nil {
		return fmt.Errorf("cue-yaml marshaling failed: %w", err)
	}

	loader := openapi3.NewLoader()
	oT, err := loader.LoadFromData([]byte(str))

	gostr, err := codegen.Generate(oT, lin.Name(), codegen.Options{
		GenerateTypes: true,
		SkipPrune:     true,
		SkipFmt:       true,
		UserTemplates: map[string]string{
			"imports.tmpl": fmt.Sprintf(tmplImports, ls.relpath),
			"typedef.tmpl": tmplTypedef,
		},
	})
	if err != nil {
		return fmt.Errorf("openapi generation failed: %w", err)
	}

	vars := goPkg{
		Name:        lin.Name(),
		LineagePath: ls.relpath,
		LatestSeqv:  sch.Version()[0],
		LatestSchv:  sch.Version()[1],
	}
	var buuf bytes.Buffer
	err = tmplAddenda.Execute(&buuf, vars)
	if err != nil {
		panic(err)
	}

	fset := token.NewFileSet()
	gf, err := parser.ParseFile(fset, "coremodel_gen.go", gostr+buuf.String(), parser.ParseComments)
	if err != nil {
		return fmt.Errorf("generated go file parsing failed: %w", err)
	}
	m := makeReplacer(lin.Name())
	ast.Walk(m, gf)

	var buf bytes.Buffer
	err = format.Node(&buf, fset, gf)
	if err != nil {
		return fmt.Errorf("ast printing failed: %w", err)
	}

	byt, err := imports.Process("coremodel_gen.go", buf.Bytes(), nil)
	if err != nil {
		return fmt.Errorf("goimports processing failed: %w", err)
	}

	err = ioutil.WriteFile(filepath.Join(path, "coremodel_gen.go"), byt, 0644)
	if err != nil {
		return fmt.Errorf("error writing generated code to file: %s", err)
	}

	return nil
}

func makeReplacer(name string) modelReplacer {
	return modelReplacer(fmt.Sprintf("%s%s", string(strings.ToUpper(name)[0]), name[1:]))
}

type modelReplacer string

func (m modelReplacer) Visit(n ast.Node) ast.Visitor {
	switch x := n.(type) {
	case *ast.Ident:
		x.Name = m.replacePrefix(x.Name)
	}
	return m
}

func (m modelReplacer) replacePrefix(str string) string {
	if len(str) >= len(m) && str[:len(m)] == string(m) {
		return strings.Replace(str, string(m), "Model", 1)
	}
	return str
}

func generateTypescript(path string, ls linsrc) error {
	schv := thema.SchemaP(ls.lin, thema.LatestVersion(ls.lin)).UnwrapCUE()

	parts, err := cuetsy.GenerateAST(schv, cuetsy.Config{})
	if err != nil {
		return fmt.Errorf("cuetsy parts gen failed: %w", err)
	}

	top, err := cuetsy.GenerateSingleAST(string(makeReplacer(ls.lin.Name())), schv, cuetsy.TypeInterface)
	if err != nil {
		return fmt.Errorf("cuetsy top gen failed: %w", err)
	}

	// TODO until cuetsy can toposort its outputs, put the top/parent type at the bottom of the file.
	// parts.Nodes = append([]ts.Decl{top.T, top.D}, parts.Nodes...)
	parts.Nodes = append(parts.Nodes, top.T, top.D)
	str := fmt.Sprintf(genHeader, ls.relpath) + fmt.Sprint(parts)

	// Ensure parent directory exists
	if _, err = os.Stat(path); os.IsNotExist(err) {
		if err = os.Mkdir(path, os.ModePerm); err != nil {
			return fmt.Errorf("error while creating parent dir (%s) for typescript model gen: %w", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("could not stat parent dir (%s) for typescript model gen: %w", path, err)
	}

	err = ioutil.WriteFile(filepath.Join(path, ls.lin.Name()+".gen.ts"), []byte(str), 0644)
	if err != nil {
		return fmt.Errorf("error writing generated typescript model: %w", err)
	}

	return nil
}

func generateRegistry(path string, lins []linsrc) error {
	type cmlist struct {
		Coremodels []linsrc
	}

	cml := cmlist{
		Coremodels: lins,
	}

	var buf bytes.Buffer
	err := tmplRegistry.Execute(&buf, cml)
	if err != nil {
		return fmt.Errorf("failed generating template: %w", err)
	}

	byt, err := imports.Process(path, buf.Bytes(), nil)
	if err != nil {
		return fmt.Errorf("goimports processing failed: %w", err)
	}

	err = ioutil.WriteFile(path, byt, 0644)
	if err != nil {
		return fmt.Errorf("error writing generated code to file: %s", err)
	}

	return nil
}

type goPkg struct {
	Name                   string
	LineagePath            string
	LatestSeqv, LatestSchv uint
	IsComposed             bool
}

var genHeader = `// This file is autogenerated. DO NOT EDIT.
//
// Derived from the Thema lineage at %s

`

var tmplImports = genHeader + `package {{ .PackageName }}

import (
	"embed"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/deepmap/oapi-codegen/pkg/runtime"
	openapi_types "github.com/deepmap/oapi-codegen/pkg/types"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/grafana/thema"
	"github.com/grafana/grafana/pkg/cuectx"
)
`

var tmplAddenda = template.Must(template.New("addenda").Parse(`
//go:embed lineage.cue
var cueFS embed.FS

// codegen ensures that this is always the latest Thema schema version
var currentVersion = thema.SV({{ .LatestSeqv }}, {{ .LatestSchv }})

// Lineage returns the Thema lineage representing a Grafana {{ .Name }}.
//
// The lineage is the canonical specification of the current {{ .Name }} schema,
// all prior schema versions, and the mappings that allow migration between
// schema versions.
{{- if .IsComposed }}//
// This is the base variant of the schema. It does not include any composed
// plugin schemas.{{ end }}
func Lineage(lib thema.Library, opts ...thema.BindOption) (thema.Lineage, error) {
	return cuectx.LoadGrafanaInstancesWithThema(filepath.Join("pkg", "coremodel", "dashboard"), cueFS, lib, opts...)
}

var _ thema.LineageFactory = Lineage

// Coremodel contains the foundational schema declaration for {{ .Name }}s.
type Coremodel struct {
	lin thema.Lineage
}

// Lineage returns the canonical dashboard Lineage.
func (c *Coremodel) Lineage() thema.Lineage {
	return c.lin
}

// CurrentSchema returns the current (latest) {{ .Name }} Thema schema.
func (c *Coremodel) CurrentSchema() thema.Schema {
	return thema.SchemaP(c.lin, currentVersion)
}

// GoType returns a pointer to an empty Go struct that corresponds to
// the current Thema schema.
func (c *Coremodel) GoType() interface{} {
	return &Model{}
}

func ProvideCoremodel(lib thema.Library) (*Coremodel, error) {
	lin, err := Lineage(lib)
	if err != nil {
		return nil, err
	}

	return &Coremodel{
		lin: lin,
	}, nil
}
`))

var tmplTypedef = `{{range .Types}}
{{ with .Schema.Description }}{{ . }}{{ else }}// {{.TypeName}} defines model for {{.JsonName}}.{{ end }}
//
// THIS TYPE IS INTENDED FOR INTERNAL USE BY THE GRAFANA BACKEND, AND IS SUBJECT TO BREAKING CHANGES.
// Equivalent Go types at stable import paths are provided in https://github.com/grafana/grok.
type {{.TypeName}} {{if and (opts.AliasTypes) (.CanAlias)}}={{end}} {{.Schema.TypeDecl}}
{{end}}
`

var tmplRegistry = template.Must(template.New("registry").Parse(`
// This file is autogenerated. DO NOT EDIT.
//
// Generated by pkg/framework/coremodel/gen.go

package staticregistry

import (
	"sync"

	"github.com/google/wire"
	{{range .Coremodels }}
	"github.com/grafana/grafana/pkg/coremodel/{{ .PkgName }}"{{end}}
	"github.com/grafana/grafana/pkg/cuectx"
	"github.com/grafana/grafana/pkg/framework/coremodel"
	"github.com/grafana/thema"
)

// CoremodelSet contains all of the wire-style providers from coremodels.
var CoremodelSet = wire.NewSet({{range .Coremodels }}
	{{ .PkgName }}.ProvideCoremodel,{{end}}
	ProvideExplicitRegistry,
	ProvideRegistry,
)

var (
	eregOnce       sync.Once
	defaultEReg    ExplicitRegistry
	defaultERegErr error

	regOnce       sync.Once
	defaultReg    *coremodel.Registry
	defaultRegErr error
)

// ExplicitRegistry provides access to individual coremodels via explicit
// method calls, which are friendly to static analysis.
type ExplicitRegistry interface {
	Dashboard() *dashboard.Coremodel
}

type explicitRegistry struct {
	{{range .Coremodels }}
	{{ .PkgName }} *{{ .PkgName }}.Coremodel{{end}}
}

{{range .Coremodels }}
func (er explicitRegistry) {{ .TitleName }}() *{{ .PkgName }}.Coremodel {
	return er.{{ .PkgName }}
}
{{end}}
func provideExplicitRegistry(lib *thema.Library) (ExplicitRegistry, error) {
	if lib == nil {
		eregOnce.Do(func() {
			defaultEReg, defaultERegErr = doProvideExplicitRegistry(cuectx.ProvideThemaLibrary())
		})
		return defaultEReg, defaultERegErr
	}

	return doProvideExplicitRegistry(*lib)
}

func doProvideExplicitRegistry(lib thema.Library) (ExplicitRegistry, error) {
	var err error
	reg := explicitRegistry{}

{{range .Coremodels }}
	reg.{{ .PkgName }}, err = {{ .PkgName }}.ProvideCoremodel(lib)
	if err != nil {
		return nil, err
	}
{{end}}

	return reg, nil
}

func provideRegistry() (*coremodel.Registry, error) {
	ereg, err := provideExplicitRegistry(nil)
	if err != nil {
		return nil, err
	}

	regOnce.Do(func() {
		defaultReg, defaultRegErr = doProvideRegistry(ereg)
	})
	return defaultReg, defaultRegErr
}

func doProvideRegistry(ereg ExplicitRegistry) (*coremodel.Registry, error) {
	return coremodel.NewRegistry({{ range .Coremodels }}
		ereg.{{ .TitleName }}(),{{ end }}
	)
}
`))
