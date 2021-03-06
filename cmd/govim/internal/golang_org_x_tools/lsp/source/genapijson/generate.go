// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command genapijson generates JSON describing gopls' external-facing API,
// including user settings and commands.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"strings"
	"time"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/mod"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/source"
)

var (
	output = flag.String("output", "", "output file")
)

func main() {
	flag.Parse()
	if err := doMain(); err != nil {
		fmt.Fprintf(os.Stderr, "Generation failed: %v\n", err)
		os.Exit(1)
	}
}

func doMain() error {
	out := os.Stdout
	if *output != "" {
		var err error
		out, err = os.OpenFile(*output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0777)
		if err != nil {
			return err
		}
		defer out.Close()
	}

	content, err := generate()
	if err != nil {
		return err
	}
	if _, err := out.Write(content); err != nil {
		return err
	}

	return out.Close()
}

func generate() ([]byte, error) {
	pkgs, err := packages.Load(
		&packages.Config{
			Mode: packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedDeps,
		},
		"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/source",
	)
	if err != nil {
		return nil, err
	}
	pkg := pkgs[0]

	api := &source.APIJSON{
		Options: map[string][]*source.OptionJSON{},
	}
	defaults := source.DefaultOptions()
	for _, cat := range []reflect.Value{
		reflect.ValueOf(defaults.DebuggingOptions),
		reflect.ValueOf(defaults.UserOptions),
		reflect.ValueOf(defaults.ExperimentalOptions),
	} {
		opts, err := loadOptions(cat, pkg)
		if err != nil {
			return nil, err
		}
		catName := strings.TrimSuffix(cat.Type().Name(), "Options")
		api.Options[catName] = opts
	}

	api.Commands, err = loadCommands(pkg)
	if err != nil {
		return nil, err
	}
	api.Lenses = loadLenses(api.Commands)

	// Transform the internal command name to the external command name.
	for _, c := range api.Commands {
		c.Command = source.CommandPrefix + c.Command
	}

	marshaled, err := json.Marshal(api)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(nil)
	fmt.Fprintf(buf, "// Code generated by \"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/source/genapijson\"; DO NOT EDIT.\n\npackage source\n\nconst GeneratedAPIJSON = %q\n", string(marshaled))
	return buf.Bytes(), nil
}

func loadOptions(category reflect.Value, pkg *packages.Package) ([]*source.OptionJSON, error) {
	// Find the type information and ast.File corresponding to the category.
	optsType := pkg.Types.Scope().Lookup(category.Type().Name())
	if optsType == nil {
		return nil, fmt.Errorf("could not find %v in scope %v", category.Type().Name(), pkg.Types.Scope())
	}

	file, err := fileForPos(pkg, optsType.Pos())
	if err != nil {
		return nil, err
	}

	enums, err := loadEnums(pkg)
	if err != nil {
		return nil, err
	}

	var opts []*source.OptionJSON
	optsStruct := optsType.Type().Underlying().(*types.Struct)
	for i := 0; i < optsStruct.NumFields(); i++ {
		// The types field gives us the type.
		typesField := optsStruct.Field(i)
		path, _ := astutil.PathEnclosingInterval(file, typesField.Pos(), typesField.Pos())
		if len(path) < 2 {
			return nil, fmt.Errorf("could not find AST node for field %v", typesField)
		}
		// The AST field gives us the doc.
		astField, ok := path[1].(*ast.Field)
		if !ok {
			return nil, fmt.Errorf("unexpected AST path %v", path)
		}

		// The reflect field gives us the default value.
		reflectField := category.FieldByName(typesField.Name())
		if !reflectField.IsValid() {
			return nil, fmt.Errorf("could not find reflect field for %v", typesField.Name())
		}

		// Format the default value. VSCode exposes settings as JSON, so showing them as JSON is reasonable.
		def := reflectField.Interface()
		// Durations marshal as nanoseconds, but we want the stringy versions, e.g. "100ms".
		if t, ok := def.(time.Duration); ok {
			def = t.String()
		}
		defBytes, err := json.Marshal(def)
		if err != nil {
			return nil, err
		}

		// Nil values format as "null" so print them as hardcoded empty values.
		switch reflectField.Type().Kind() {
		case reflect.Map:
			if reflectField.IsNil() {
				defBytes = []byte("{}")
			}
		case reflect.Slice:
			if reflectField.IsNil() {
				defBytes = []byte("[]")
			}
		}

		typ := typesField.Type().String()
		if _, ok := enums[typesField.Type()]; ok {
			typ = "enum"
		}

		opts = append(opts, &source.OptionJSON{
			Name:       lowerFirst(typesField.Name()),
			Type:       typ,
			Doc:        lowerFirst(astField.Doc.Text()),
			Default:    string(defBytes),
			EnumValues: enums[typesField.Type()],
		})
	}
	return opts, nil
}

func loadEnums(pkg *packages.Package) (map[types.Type][]source.EnumValue, error) {
	enums := map[types.Type][]source.EnumValue{}
	for _, name := range pkg.Types.Scope().Names() {
		obj := pkg.Types.Scope().Lookup(name)
		cnst, ok := obj.(*types.Const)
		if !ok {
			continue
		}
		f, err := fileForPos(pkg, cnst.Pos())
		if err != nil {
			return nil, fmt.Errorf("finding file for %q: %v", cnst.Name(), err)
		}
		path, _ := astutil.PathEnclosingInterval(f, cnst.Pos(), cnst.Pos())
		spec := path[1].(*ast.ValueSpec)
		value := cnst.Val().ExactString()
		doc := valueDoc(cnst.Name(), value, spec.Doc.Text())
		v := source.EnumValue{
			Value: value,
			Doc:   doc,
		}
		enums[obj.Type()] = append(enums[obj.Type()], v)
	}
	return enums, nil
}

// valueDoc transforms a docstring documenting an constant identifier to a
// docstring documenting its value.
//
// If doc is of the form "Foo is a bar", it returns '`"fooValue"` is a bar'. If
// doc is non-standard ("this value is a bar"), it returns '`"fooValue"`: this
// value is a bar'.
func valueDoc(name, value, doc string) string {
	if doc == "" {
		return ""
	}
	if strings.HasPrefix(doc, name) {
		// docstring in standard form. Replace the subject with value.
		return fmt.Sprintf("`%s`%s", value, doc[len(name):])
	}
	return fmt.Sprintf("`%s`: %s", value, doc)
}

func loadCommands(pkg *packages.Package) ([]*source.CommandJSON, error) {
	// The code that defines commands is much more complicated than the
	// code that defines options, so reading comments for the Doc is very
	// fragile. If this causes problems, we should switch to a dynamic
	// approach and put the doc in the Commands struct rather than reading
	// from the source code.

	// Find the Commands slice.
	typesSlice := pkg.Types.Scope().Lookup("Commands")
	f, err := fileForPos(pkg, typesSlice.Pos())
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(f, typesSlice.Pos(), typesSlice.Pos())
	vspec := path[1].(*ast.ValueSpec)
	var astSlice *ast.CompositeLit
	for i, name := range vspec.Names {
		if name.Name == "Commands" {
			astSlice = vspec.Values[i].(*ast.CompositeLit)
		}
	}

	var commands []*source.CommandJSON

	// Parse the objects it contains.
	for _, elt := range astSlice.Elts {
		// Find the composite literal of the Command.
		typesCommand := pkg.TypesInfo.ObjectOf(elt.(*ast.Ident))
		path, _ := astutil.PathEnclosingInterval(f, typesCommand.Pos(), typesCommand.Pos())
		vspec := path[1].(*ast.ValueSpec)

		var astCommand ast.Expr
		for i, name := range vspec.Names {
			if name.Name == typesCommand.Name() {
				astCommand = vspec.Values[i]
			}
		}

		// Read the Name and Title fields of the literal.
		var name, title string
		ast.Inspect(astCommand, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if ok {
				k := kv.Key.(*ast.Ident).Name
				switch k {
				case "Name":
					name = strings.Trim(kv.Value.(*ast.BasicLit).Value, `"`)
				case "Title":
					title = strings.Trim(kv.Value.(*ast.BasicLit).Value, `"`)
				}
			}
			return true
		})

		if title == "" {
			title = name
		}

		// Conventionally, the doc starts with the name of the variable.
		// Replace it with the name of the command.
		doc := vspec.Doc.Text()
		doc = strings.Replace(doc, typesCommand.Name(), name, 1)

		commands = append(commands, &source.CommandJSON{
			Command: name,
			Title:   title,
			Doc:     doc,
		})
	}
	return commands, nil
}

func loadLenses(commands []*source.CommandJSON) []*source.LensJSON {
	lensNames := map[string]struct{}{}
	for k := range source.LensFuncs() {
		lensNames[k] = struct{}{}
	}
	for k := range mod.LensFuncs() {
		lensNames[k] = struct{}{}
	}

	var lenses []*source.LensJSON

	for _, cmd := range commands {
		if _, ok := lensNames[cmd.Command]; ok {
			lenses = append(lenses, &source.LensJSON{
				Lens:  cmd.Command,
				Title: cmd.Title,
				Doc:   cmd.Doc,
			})
		}
	}
	return lenses
}

func lowerFirst(x string) string {
	if x == "" {
		return x
	}
	return strings.ToLower(x[:1]) + x[1:]
}

func fileForPos(pkg *packages.Package, pos token.Pos) (*ast.File, error) {
	fset := pkg.Fset
	for _, f := range pkg.Syntax {
		if fset.Position(f.Pos()).Filename == fset.Position(pos).Filename {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no file for pos %v", pos)
}
