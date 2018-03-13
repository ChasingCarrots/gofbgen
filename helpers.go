package gofbgen

import (
	"bytes"
	"strings"
)

type typePath struct {
	pkg, name, importPath string
}

// splitTypePath takes a path of the form a/b/c/d and splits it into three parts:
// The name of the type described by the path (d), the package of the path (c),
// and the import path (a/b/c).
func splitTypePath(p string) typePath {
	nameIdx := strings.LastIndex(p, "/")
	importPath := p[:nameIdx]
	pkgIdx := strings.LastIndex(importPath, "/")
	var pkgName string
	if pkgIdx >= 0 {
		pkgName = importPath[pkgIdx+1:]
	} else {
		pkgName = importPath
	}

	return typePath{
		pkg:        pkgName,
		importPath: importPath,
		name:       p[nameIdx+1:],
	}
}

func isPrimitiveType(typeName string) bool {
	switch typeName {
	case "bool":
		fallthrough
	case "string":
		fallthrough
	case "int", "int8", "int16", "int32", "int64":
		fallthrough
	case "uint", "uint8", "uint16", "uint32", "uint64":
		fallthrough
	case "uintptr":
		fallthrough
	case "byte":
		fallthrough
	case "rune":
		fallthrough
	case "float32", "float64":
		fallthrough
	case "complex64", "complex128":
		return true
	default:
		return false
	}
}

func write(buf *bytes.Buffer, values ...string) {
	for _, v := range values {
		buf.WriteString(v)
	}
}
