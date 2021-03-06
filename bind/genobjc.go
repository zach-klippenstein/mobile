// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import (
	"fmt"
	"go/token"
	"golang.org/x/tools/go/types"
	"log"
	"strings"
	"unicode"
	"unicode/utf8"
)

type objcGen struct {
	*printer
	fset *token.FileSet
	pkg  *types.Package
	err  ErrorList

	// fields set by init.
	pkgName    string
	namePrefix string
	funcs      []*types.Func
	names      []*types.TypeName
}

func capitalize(n string) string {
	firstRune, size := utf8.DecodeRuneInString(n)
	return string(unicode.ToUpper(firstRune)) + n[size:]
}

func (g *objcGen) init() {
	g.pkgName = g.pkg.Name()
	g.namePrefix = "Go" + capitalize(g.pkgName)
	g.funcs = nil
	g.names = nil

	scope := g.pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types.Func:
			g.funcs = append(g.funcs, obj)
		case *types.TypeName:
			g.names = append(g.names, obj)
			// TODO(hyangah): *types.Const, *types.Var
		}
	}
}

const objcPreamble = `// Objective-C API for talking to %s Go package.
//   gobind -lang=objc %s
//
// File is generated by gobind. Do not edit.

`

func (g *objcGen) genH() error {
	g.init()

	g.Printf(objcPreamble, g.pkg.Path(), g.pkg.Path())
	g.Printf("#ifndef __Go%s_H__\n", capitalize(g.pkgName))
	g.Printf("#define __Go%s_H__\n", capitalize(g.pkgName))
	g.Printf("\n")
	g.Printf(`#include <Foundation/Foundation.h>`)
	g.Printf("\n\n")

	// @class names
	for _, obj := range g.names {
		named := obj.Type().(*types.Named)
		switch named.Underlying().(type) {
		case *types.Struct, *types.Interface:
			g.Printf("@class %s%s;\n", g.namePrefix, obj.Name())
		}
		g.Printf("\n")
	}

	// @interfaces
	for _, obj := range g.names {
		named := obj.Type().(*types.Named)
		switch t := named.Underlying().(type) {
		case *types.Struct:
			g.genStructH(obj, t)
		case *types.Interface:
			g.genInterfaceH(obj, t)
		}
		g.Printf("\n")
	}

	// static functions.
	for _, obj := range g.funcs {
		g.genFuncH(obj)
		g.Printf("\n")
	}

	// declare all named types first.
	g.Printf("#endif\n")

	if len(g.err) > 0 {
		return g.err
	}
	return nil
}

func (g *objcGen) genM() error {
	g.init()

	g.Printf(objcPreamble, g.pkg.Path(), g.pkg.Path())
	g.Printf("#include %q\n", g.namePrefix+".h")
	g.Printf("#include <Foundation/Foundation.h>\n")
	g.Printf("#include \"seq.h\"\n")
	g.Printf("\n")
	g.Printf("static NSString *errDomain = @\"go.%s\";\n", g.pkg.Path())
	g.Printf("\n")

	g.Printf("#define _DESCRIPTOR_ %q\n\n", g.pkgName)
	for i, obj := range g.funcs {
		g.Printf("#define _CALL_%s_ %d\n", obj.Name(), i+1)
	}
	g.Printf("\n")

	// @implementation Go*_* : GoSeqProxyObject
	for _, obj := range g.names {
		named := obj.Type().(*types.Named)
		switch t := named.Underlying().(type) {
		case *types.Struct:
			g.genStructM(obj, t)
		case *types.Interface:
			g.genInterfaceM(obj, t)
		}
		g.Printf("\n")
	}

	// global functions.
	for _, obj := range g.funcs {
		g.genFuncM(obj)
		g.Printf("\n")
	}

	if len(g.err) > 0 {
		return g.err
	}
	return nil
}

type funcSummary struct {
	name              string
	ret               string
	params, retParams []paramInfo
}

type paramInfo struct {
	typ  types.Type
	name string
}

func (g *objcGen) funcSummary(obj *types.Func) *funcSummary {
	s := &funcSummary{name: obj.Name()}

	sig := obj.Type().(*types.Signature)
	params := sig.Params()
	for i := 0; i < params.Len(); i++ {
		p := params.At(i)
		v := paramInfo{
			typ:  p.Type(),
			name: paramName(params, i),
		}
		s.params = append(s.params, v)
	}

	res := sig.Results()
	switch res.Len() {
	case 0:
		s.ret = "void"
	case 1:
		p := res.At(0)
		if isErrorType(p.Type()) {
			s.retParams = append(s.retParams, paramInfo{
				typ:  p.Type(),
				name: "error",
			})
			s.ret = "BOOL"
		} else {
			name := p.Name()
			if name == "" || paramRE.MatchString(name) {
				name = "ret0_"
			}
			typ := p.Type()
			s.retParams = append(s.retParams, paramInfo{typ: typ, name: name})
			s.ret = g.objcType(typ)
		}
	case 2:
		name := res.At(0).Name()
		if name == "" || paramRE.MatchString(name) {
			name = "ret0_"
		}
		s.retParams = append(s.retParams, paramInfo{
			typ:  res.At(0).Type(),
			name: name,
		})

		if !isErrorType(res.At(1).Type()) {
			g.errorf("second result value must be of type error: %s", obj)
			return nil
		}
		s.retParams = append(s.retParams, paramInfo{
			typ:  res.At(1).Type(),
			name: "error", // TODO(hyangah): name collision check.
		})
		s.ret = "BOOL"
	default:
		// TODO(hyangah): relax the constraint on multiple return params.
		g.errorf("too many result values: %s", obj)
		return nil
	}

	return s
}

func (s *funcSummary) asFunc(g *objcGen) string {
	var params []string
	for _, p := range s.params {
		params = append(params, g.objcType(p.typ)+" "+p.name)
	}
	if !s.returnsVal() {
		for _, p := range s.retParams {
			params = append(params, g.objcType(p.typ)+"* "+p.name)
		}
	}
	return fmt.Sprintf("%s %s%s(%s)", s.ret, g.namePrefix, s.name, strings.Join(params, ", "))
}

func (s *funcSummary) asMethod(g *objcGen) string {
	var params []string
	for i, p := range s.params {
		var key string
		if i != 0 {
			key = p.name
		}
		params = append(params, fmt.Sprintf("%s:(%s)%s", key, g.objcType(p.typ), p.name))
	}
	if !s.returnsVal() {
		for _, p := range s.retParams {
			var key string
			if len(params) > 0 {
				key = p.name
			}
			params = append(params, fmt.Sprintf("%s:(%s)%s", key, g.objcType(p.typ)+"*", p.name))
		}
	}
	return fmt.Sprintf("(%s)%s%s", s.ret, s.name, strings.Join(params, " "))
}

func (s *funcSummary) returnsVal() bool {
	return len(s.retParams) == 1 && !isErrorType(s.retParams[0].typ)
}

func (g *objcGen) genFuncH(obj *types.Func) {
	if s := g.funcSummary(obj); s != nil {
		g.Printf("FOUNDATION_EXPORT %s;\n", s.asFunc(g))
	}
}

func (g *objcGen) seqType(typ types.Type) string {
	s := seqType(typ)
	if s == "String" {
		// TODO(hyangah): non utf-8 strings.
		s = "UTF8"
	}
	return s
}

func (g *objcGen) genFuncM(obj *types.Func) {
	s := g.funcSummary(obj)
	if s == nil {
		return
	}
	g.Printf("%s {\n", s.asFunc(g))
	g.Indent()
	g.genFunc("_DESCRIPTOR_", fmt.Sprintf("_CALL_%s_", s.name), s, false)
	g.Outdent()
	g.Printf("}\n")
}

func (g *objcGen) genFunc(pkgDesc, callDesc string, s *funcSummary, isMethod bool) {
	g.Printf("GoSeq in_ = {};\n")
	g.Printf("GoSeq out_ = {};\n")
	if isMethod {
		g.Printf("go_seq_writeRef(&in_, self.ref);\n")
	}
	for _, p := range s.params {
		st := g.seqType(p.typ)
		if st == "Ref" {
			g.Printf("go_seq_write%s(&in_, %s.ref);\n", st, p.name)
		} else {
			g.Printf("go_seq_write%s(&in_, %s);\n", st, p.name)
		}
	}
	g.Printf("go_seq_send(%s, %s, &in_, &out_);\n", pkgDesc, callDesc)

	if s.returnsVal() {
		p := s.retParams[0]
		if seqTyp := g.seqType(p.typ); seqTyp != "Ref" {
			g.Printf("%s %s = go_seq_read%s(&out_);\n", g.objcType(p.typ), p.name, g.seqType(p.typ))
		} else {
			ptype := g.objcType(p.typ)
			g.Printf("GoSeqRef* %s_ref = go_seq_readRef(&out_);\n", p.name)
			g.Printf("%s %s = %s_ref.obj;\n", ptype, p.name, p.name)
			g.Printf("if (%s == NULL) {\n", p.name)
			g.Indent()
			g.Printf("%s = [[%s alloc] initWithRef:%s_ref];\n", p.name, ptype[:len(ptype)-1], p.name)
			g.Outdent()
			g.Printf("}\n")
		}
	} else {
		for _, p := range s.retParams {
			if isErrorType(p.typ) {
				g.Printf("NSString* _%s = go_seq_readUTF8(&out_);\n", p.name)
				g.Printf("if ([_%s length] != 0 && %s != nil) {\n", p.name, p.name)
				g.Indent()
				g.Printf("NSMutableDictionary *details = [NSMutableDictionary dictionary];\n")
				g.Printf("[details setValue:_%s forKey:NSLocalizedDescriptionKey];\n", p.name)
				g.Printf("*%s = [NSError errorWithDomain:errDomain code:1 userInfo:details];\n", p.name)
				g.Outdent()
				g.Printf("}\n")
			} else if seqTyp := g.seqType(p.typ); seqTyp != "Ref" {
				g.Printf("%s %s_val = go_seq_read%s(&out_);\n", g.objcType(p.typ), p.name, g.seqType(p.typ))
				g.Printf("if (%s != NULL) {\n", p.name)
				g.Indent()
				g.Printf("*%s = %s_val;\n", p.name, p.name)
				g.Outdent()
				g.Printf("}\n")
			} else {
				ptype := g.objcType(p.typ)
				g.Printf("GoSeqRef* %s_ref = go_seq_readRef(&out_);\n", p.name)
				g.Printf("if (%s != NULL) {\n", p.name)
				g.Indent()
				g.Printf("*%s = %s_ref.obj;\n", p.name, p.name)
				g.Printf("if (*%s == NULL) {\n", p.name)
				g.Indent()
				g.Printf("*%s = [[%s alloc] initWithRef:%s_ref];\n", p.name, ptype[:len(ptype)-1], p.name)
				g.Outdent()
				g.Printf("}\n")
				g.Outdent()
				g.Printf("}\n")
			}
		}
	}

	g.Printf("go_seq_free(&in_);\n")
	g.Printf("go_seq_free(&out_);\n")
	if n := len(s.retParams); n > 0 {
		p := s.retParams[n-1]
		if isErrorType(p.typ) {
			g.Printf("return ([_%s length] == 0);\n", p.name)
		} else {
			g.Printf("return %s;\n", p.name)
		}
	}
}

func (g *objcGen) genInterfaceH(obj *types.TypeName, t *types.Interface) {
	log.Printf("TODO: %s", obj.Name())
}
func (g *objcGen) genInterfaceM(obj *types.TypeName, t *types.Interface) {
	log.Printf("TODO: %s", obj.Name())
}

func (g *objcGen) genStructH(obj *types.TypeName, t *types.Struct) {
	g.Printf("@interface %s%s : NSObject {\n", g.namePrefix, obj.Name())
	g.Printf("}\n")
	g.Printf("@property(strong, readonly) id ref;\n")
	g.Printf("\n")
	g.Printf("- (id)initWithRef:(id)ref;\n")

	// accessors to exported fields.
	for _, f := range exportedFields(t) {
		// TODO(hyangah): error type field?
		name, typ := f.Name(), g.objcType(f.Type())
		g.Printf("- (%s)%s;\n", typ, name)
		g.Printf("- (void)set%s:(%s)v;\n", name, typ)
	}

	// exported methods
	for _, m := range exportedMethodSet(types.NewPointer(obj.Type())) {
		s := g.funcSummary(m)
		g.Printf("- %s;\n", s.asMethod(g))
	}
	g.Printf("@end\n")
}

func (g *objcGen) genStructM(obj *types.TypeName, t *types.Struct) {
	fields := exportedFields(t)
	methods := exportedMethodSet(types.NewPointer(obj.Type()))

	desc := fmt.Sprintf("_GO_%s_%s", g.pkgName, obj.Name())
	g.Printf("#define %s_DESCRIPTOR_ \"go.%s.%s\"\n", desc, g.pkgName, obj.Name())
	for i, f := range fields {
		g.Printf("#define %s_FIELD_%s_GET_ (0x%x0f)\n", desc, f.Name(), i)
		g.Printf("#define %s_FIELD_%s_SET_ (0x%x1f)\n", desc, f.Name(), i)
	}
	for i, m := range methods {
		g.Printf("#define %s_%s_ (0x%x0c)\n", desc, m.Name(), i)
	}

	g.Printf("\n")
	g.Printf("@implementation %s%s {\n", g.namePrefix, obj.Name())
	g.Printf("}\n\n")
	g.Printf("- (id)initWithRef:(id)ref {\n")
	g.Indent()
	g.Printf("self = [super init];\n")
	g.Printf("if (self) { _ref = ref; }\n")
	g.Printf("return self;\n")
	g.Outdent()
	g.Printf("}\n\n")

	for _, f := range fields {
		// getter
		// TODO(hyangah): support error type fields?
		s := &funcSummary{
			name: f.Name(),
			ret:  g.objcType(f.Type()),
		}
		s.retParams = append(s.retParams, paramInfo{typ: f.Type(), name: "ret_"})

		g.Printf("- %s {\n", s.asMethod(g))
		g.Indent()
		g.genFunc(desc+"_DESCRIPTOR_", desc+"_FIELD_"+f.Name()+"_GET_", s, true)
		g.Outdent()
		g.Printf("}\n\n")

		// setter
		s = &funcSummary{
			name: "set" + f.Name(),
			ret:  "void",
		}
		s.params = append(s.params, paramInfo{typ: f.Type(), name: "v"})

		g.Printf("- %s {\n", s.asMethod(g))
		g.Indent()
		g.genFunc(desc+"_DESCRIPTOR_", desc+"_FIELD_"+f.Name()+"_SET_", s, true)
		g.Outdent()
		g.Printf("}\n\n")
	}

	for _, m := range methods {
		s := g.funcSummary(m)
		g.Printf("- %s {\n", s.asMethod(g))
		g.Indent()
		g.genFunc(desc+"_DESCRIPTOR_", desc+"_"+m.Name()+"_", s, true)
		g.Outdent()
		g.Printf("}\n\n")
	}
	g.Printf("@end\n")
}

func (g *objcGen) errorf(format string, args ...interface{}) {
	g.err = append(g.err, fmt.Errorf(format, args...))
}

func (g *objcGen) objcType(typ types.Type) string {
	if isErrorType(typ) {
		return "NSError*"
	}

	switch typ := typ.(type) {
	case *types.Basic:
		switch typ.Kind() {
		case types.Bool:
			return "BOOL"
		case types.Int:
			return "int"
		case types.Int8:
			return "int8_t"
		case types.Int16:
			return "int16_t"
		case types.Int32:
			return "int32_t"
		case types.Int64:
			return "int64_t"
		case types.Uint8:
			// byte is an alias of uint8, and the alias is lost.
			return "byte"
		case types.Uint16:
			return "uint16_t"
		case types.Uint32:
			return "uint32_t"
		case types.Uint64:
			return "uint64_t"
		case types.Float32:
			return "float"
		case types.Float64:
			return "double"
		case types.String:
			return "NSString*"
		default:
			g.errorf("unsupported type: %s", typ)
			return "TODO"
		}
	case *types.Slice:
		elem := g.objcType(typ.Elem())
		// Special case: NSData seems to be a better option for byte slice.
		if elem == "byte" {
			return "NSData*"
		}
		// TODO(hyangah): support other slice types: NSArray or CFArrayRef.
		// Investigate the performance implication.
		g.errorf("unsupported type: %s", typ)
		return "TODO"
	case *types.Pointer:
		if _, ok := typ.Elem().(*types.Named); ok {
			return g.objcType(typ.Elem()) + "*"
		}
		g.errorf("unsupported pointer to type: %s", typ)
		return "TODO"
	case *types.Named:
		n := typ.Obj()
		if n.Pkg() != g.pkg {
			g.errorf("type %s is in package %s; only types defined in package %s is supported", n.Name(), n.Pkg().Name(), g.pkg.Name())
			return "TODO"
		}
		switch typ.Underlying().(type) {
		case *types.Interface:
			return g.namePrefix + n.Name() + "*"
		case *types.Struct:
			return g.namePrefix + n.Name()
		}
		g.errorf("unsupported, named type %s", typ)
		return "TODO"
	default:
		g.errorf("unsupported type: %#+v, %s", typ, typ)
		return "TODO"
	}
}
