package gofbgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"sort"

	"github.com/chasingcarrots/gotransform"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/chasingcarrots/gotransform/tagparser"
	"github.com/pkg/errors"

	"github.com/chasingcarrots/gotransform/tagproc"
)

// FlatbufferHandler is a tag handler that generates flatbuffer serialization functions
// for structs.
type FlatbufferHandler struct {
	serializationHints map[string]func(string) string
	buf                bytes.Buffer
	outputPath         string
}

// New constructs a new flatbuffer handler.
// Outputpath is the path to the file that should contain all the generated code, pkg is
// the package for this file, and imports describes the imports that should be added to
// the file.
// The serialization hints parameter maps type paths to functions that produce the serialization
// code for the given type in-place.
func New(outputPath, pkg string, imports []string, serializationHints map[string]func(string) string) *FlatbufferHandler {
	fh := &FlatbufferHandler{
		serializationHints: serializationHints,
		outputPath:         outputPath,
	}
	write(&fh.buf, "package ", pkg, "\n")
	for _, imp := range imports {
		write(&fh.buf, "import \"", imp, "\"\n")
	}
	return fh
}

// BeginFile is only implemented to satisfy the TagHandler interface.
func (*FlatbufferHandler) BeginFile(context tagproc.TagContext) error { return nil }

// FinishFile is implemented to satisfy the TagHandler interface.
func (*FlatbufferHandler) FinishFile(context tagproc.TagContext) error { return nil }

// Finalize writes out the generated code.
func (fh *FlatbufferHandler) Finalize() error {
	return gotransform.WriteGoFile(fh.outputPath, &fh.buf)
}

// HandleTag is called once for each struct with the right tag.
func (fh *FlatbufferHandler) HandleTag(context tagproc.TagContext, obj *ast.Object, tagLiteral string) error {
	// parse the field tag value associated to the tag
	tagValuesMultiple, _ := tagparser.Parse(tagLiteral)
	tagValues := tagparser.Unique(tagValuesMultiple)
	// specifies the type of the flatbuffer struct
	name, ok := tagValues["Type"]
	if !ok {
		return fmt.Errorf("Missing Type in Flatbuffer Tag in %s for %s", tagLiteral, obj.Name)
	}
	path := splitTypePath(name)
	// specifies the name of the serialization function to generate.
	function, ok := tagValues["Function"]
	if !ok {
		function = "Serialize" + path.name
	}

	astutil.AddImport(context.FileSet, context.File, path.importPath)
	astutil.AddNamedImport(context.FileSet, context.File, "flatbuffers", "github.com/google/flatbuffers/go")

	typeSpec := obj.Decl.(*ast.TypeSpec)
	struc, ok := typeSpec.Type.(*ast.StructType)
	if !ok {
		return fmt.Errorf("Can only generate flatbuffer code for structs, not for %s", obj.Name)
	}

	fbCtxt := fbContext{
		pkg:        path.pkg,
		typ:        path.name,
		buf:        &fh.buf,
		fnName:     function,
		targetName: "target",
		targetType: "*" + obj.Name,
	}
	err := fh.generateSerializationCode(struc, context.Imports, &fbCtxt)
	return errors.Wrapf(err, "Failed to generate flatbuffer code for %s", obj.Name)
}

// fbContext holds values that are frequently needed for the code generation
type fbContext struct {
	// pkg is the package that holds the flatbuffer struct for which code
	// should be generated (e.g. myflatbuffers)
	// typ is the name of the flatbuffer struct itself (e.g. msg).
	pkg, typ string
	// fnName is the name of the function that should be generated.
	// targetType is the type of the parameter that represents what should
	// be serialized in the generated function
	// targetName is the name of said parameter.
	fnName, targetType, targetName string
	// buf is the buffer within which the generated code should be written.
	buf *bytes.Buffer
}

// generateSerializationCode generates the serialization function for a struct
// and returns the offset of the serialized value in the struct.
// The imports parameter maps package names to their import path,
// context contains additional values to help with the generation.
func (fh *FlatbufferHandler) generateSerializationCode(struc *ast.StructType, imports map[string]string, context *fbContext) error {
	opts, err := fh.parseOptions(struc, imports)
	if err != nil {
		return err
	}

	write(context.buf, fmt.Sprintf("func %s(flatbuilder *flatbuffers.Builder, %s %s) flatbuffers.UOffsetT {\n", context.fnName, context.targetName, context.targetType))

	// sort all fields with serialization info such that entries that have
	// reference semantics come first; they need to be serialized before the
	// main struct is serialized.
	sort.Sort(tablesFirst(opts))

	var referenceVariables []string
	for _, opt := range opts {
		if !isTableType(opt) {
			// this is the first non-table type, restart from the beginning and
			// start building the actual buffer
			break
		}
		varName := opt.member
		if err := serializeReferenceType(context, opt, varName); err != nil {
			return err
		}
		referenceVariables = append(referenceVariables, varName)
	}

	// now do the actual serialization
	adder := mkAdder(context.buf, context.pkg, context.typ)
	write(context.buf, context.pkg, ".", context.typ, "Start(flatbuilder)\n")
	for i, opt := range opts {
		if i < len(referenceVariables) {
			// add the reference to the variable that we stored earlier
			variableName := referenceVariables[i]
			adder(opt.name, variableName)
		} else {
			// add serialization inline
			selector := context.targetName + "." + opt.member
			if opt.fn != nil {
				// do a cast or function call to compute the result
				adder(opt.name, opt.fn(selector))
			} else if opt.asType == "bool" {
				// special case for bool: it needs to be cast to byte very explicitly.
				write(context.buf, opt.member, " := byte(1)\n")
				write(context.buf, "if (", selector, ") {\n")
				write(context.buf, "\t", opt.member, " = 0\n")
				write(context.buf, "}\n")
				adder(opt.name, opt.member)
			} else {
				// just add the value itself
				adder(opt.name, selector)
			}
		}
	}
	write(context.buf, "return ", context.pkg, ".", context.typ, "End(flatbuilder)\n")
	write(context.buf, "}\n\n")
	return nil
}

// serializeReferenceType generates the code necessary to serialize a type with
// reference semantics (i.e., a table, vector, or string).
func serializeReferenceType(context *fbContext, opts *flatbufferOptions, variableName string) error {
	selector := context.targetName + "." + opts.member
	if opts.asType == "custom" || opts.asType == "table" {
		write(context.buf, variableName, " := ", opts.fn(selector), "\n")
		return nil
	} else if opts.asType == "string" {
		write(context.buf, variableName, " := flatbuilder.CreateString(", selector, ")\n")
		return nil
	} else if opts.isVector {
		isTable := opts.asType == "table"
		isString := opts.asType == "string"
		isReference := isTable || isString
		tableName := opts.member + "___tmpoffsetvec"
		if isReference {
			// if the values in the vector are of a reference type, we need to serialize them first
			// before beginning the vector
			write(context.buf, "var ", tableName, "[]flatbuffers.UOffsetT\n")
			write(context.buf, "for i := 0; i < len(", selector, "); i++ {\n")
			if isTable {
				write(context.buf, tableName, " = append(", tableName, ", "+opts.fn(selector+"[i]")+")\n")
			} else {
				write(context.buf, tableName, " = append(", tableName, ", flatbuilder.CreateString(", selector, ")\n")
			}
			write(context.buf, "}\n")
		}

		// now write out the vector itself -- in reverse, because flatbuffers only have prepend operations
		write(context.buf, context.pkg, ".", context.typ, "Start", opts.name, "Vector(flatbuilder, len(", selector, "))\n")
		write(context.buf, "for i := len(", selector, ") - 1; i >= 0; i-- {\n")
		var err error
		if isReference {
			write(context.buf, "flatbuilder.PrependUOffsetT(", tableName, "[i])")
		} else {
			err = vectorInnerFunc(context, opts, selector+"[i]")
		}
		write(context.buf, "\n}\n")
		write(context.buf, variableName, " := flatbuilder.EndVector(len(", selector, "))\n")
		return err
	}
	return errors.Errorf("Failed to serialize table for member %v", opts.member)
}

func vectorInnerFunc(context *fbContext, opts *flatbufferOptions, selector string) error {
	if fn, err := prependFn(opts.asType); err == nil {
		if opts.fn != nil {
			// primitive types as functions should still work in vectors and simply cast.
			write(context.buf, "flatbuilder.", fn, "(", opts.fn(selector), ")")
		} else {
			write(context.buf, "flatbuilder.", fn, "(", selector, ")")
		}
		return nil
	} else if opts.fn != nil {
		// call custom handler for in-place serialization
		write(context.buf, opts.fn(selector))
		return nil
	}
	return errors.Errorf("Don't know how to serialize inner part of vector for member %s in structs %s", opts.member, context.targetName)
}

func prependFn(typ string) (string, error) {
	switch typ {
	case "bool":
		return "PrependBool", nil
	case "uint8":
		return "PrependUint8", nil
	case "uint16":
		return "PrependUint16", nil
	case "uint", "uint32":
		return "PrependUint32", nil
	case "uint64":
		return "PrependUint64", nil
	case "int8":
		return "PrependInt8", nil
	case "int16":
		return "PrependInt16", nil
	case "int", "int32":
		return "PrependInt32", nil
	case "int64":
		return "PrependInt16", nil
	case "float32":
		return "PrependFloat32", nil
	case "float64":
		return "PrependFloat64", nil
	case "byte":
		return "PrependByte", nil
	default:
		return "", errors.Errorf("Don't know how to handle type %s in flatbuffer vector", typ)
	}
}

type tablesFirst []*flatbufferOptions

func (tf tablesFirst) Len() int      { return len(tf) }
func (tf tablesFirst) Swap(i, j int) { tf[i], tf[j] = tf[j], tf[i] }
func (tf tablesFirst) Less(i, j int) bool {
	tableI, tableJ := isTableType(tf[i]), isTableType(tf[j])
	if tableI && !tableJ {
		return true
	}
	return false
}

func isTableType(opts *flatbufferOptions) bool {
	return opts.isVector || opts.asType == "table" || opts.asType == "custom" || opts.asType == "string"
}

type flatbufferOptions struct {
	// The struct member that these options apply to
	member string
	// The name of the member in the flatbuffer
	name string
	// The type to serialize in. Either of a primitive type, 'table', 'custom', or a proper type path.
	asType string
	// The function to use for serializing this member (optional). This is override to a cast if a primitive
	// type is explicitly specified.
	fn func(string) string
	// Whether this is a vector or a single value
	isVector bool
}

// parseOptions searches a struct for all fields annotated with fbName tags and transforms
// it to a more convenient format for the code generator.
func (fh *FlatbufferHandler) parseOptions(struc *ast.StructType, imports map[string]string) ([]*flatbufferOptions, error) {
	var options []*flatbufferOptions
	for _, m := range struc.Fields.List {
		if m.Tag == nil {
			continue
		}
		// strip the field tag from its enclosing quotes and parse it
		kv, err := tagparser.Parse(m.Tag.Value[1 : len(m.Tag.Value)-1])
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to parse tags %v", m.Tag.Value)
		}
		params := tagparser.Unique(kv)

		// fbName is the name of the member in the flatbuffer
		fb, ok := params["fbName"]
		if !ok {
			continue
		}
		if len(m.Names) != 1 {
			return nil, errors.Errorf("Found fnName tag on a member that is not a single name, this is not supported")
		}

		// find the type path of this field and whether it is a primitive type
		isPrimitive, typePath := findType(m.Type, imports)

		// fbFunc is the function to use for serialization. If no function is specified,
		// it will look for serialization hints for the type.
		fn, hasFn := params["fbFunc"]
		var realFn func(string) string
		if !hasFn {
			realFn, hasFn = fh.serializationHints[typePath]
		} else {
			realFn = func(x string) string { return fn + "(flatbuilder, &" + x + ")" }
		}

		// fbType specifies what type this should be serialized to.
		typ, hasType := params["fbType"]
		if hasType && isPrimitiveType(typ) {
			if hasFn {
				return nil, errors.Errorf("Member %s is specified as a primitive type (%s) but has a serialization function attached.", m.Names[0], typePath)
			}
			// override function to a cast
			realFn = func(x string) string { return typ + "(" + x + ")" }
			hasFn = true
		} else if !hasType {
			typ = typePath
		}

		if !isPrimitive && !hasFn {
			return nil, errors.Errorf("Member %s is not a primitive type (%s) and has no serialization function attached.", m.Names[0], typePath)
		}
		_, isVector := m.Type.(*ast.ArrayType)

		options = append(options,
			&flatbufferOptions{
				member:   m.Names[0].Name,
				name:     fb,
				asType:   typ,
				fn:       realFn,
				isVector: isVector,
			},
		)
	}
	return options, nil
}

func findType(expr ast.Expr, imports map[string]string) (primitiveType bool, path string) {
	// we may skip an outermost array and a single indirection via pointer
	t, ok := expr.(*ast.ArrayType)
	if ok {
		expr = t.Elt
	}
	s, ok := expr.(*ast.StarExpr)
	if ok {
		expr = s.X
	}
	if selector, ok := expr.(*ast.SelectorExpr); ok {
		// assume that it has the form pkg.Name
		name := selector.Sel.Name
		importName, ok := selector.X.(*ast.Ident)
		if !ok {
			return false, ""
		}
		return true, imports[importName.Name] + "/" + name
	} else if ident, ok := expr.(*ast.Ident); ok {
		if isPrimitiveType(ident.Name) {
			return true, ident.Name
		}
		// if it is not a primitive type, prepend the current package name
		return false, imports[""] + "/" + ident.Name
	} else {
		return false, ""
	}
}

// mkAdder returns a function that adds 'Add' calls with a name to the
func mkAdder(buf *bytes.Buffer, pkg, typ string) func(name string, arguments ...string) {
	return func(name string, arguments ...string) {
		write(buf, pkg, ".", typ, "Add", name, "(flatbuilder")
		for _, arg := range arguments {
			buf.WriteString(", ")
			buf.WriteString(arg)
		}
		buf.WriteString(")\n")
	}
}
