package parser

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"path"
	"reflect"
	"strings"
)

var (
	EnableHeader = true
)

type Pkg struct {
	Alias string
	Path  string
}

func (p *Pkg) String() string {
	var s string
	if len(p.Alias) > 0 {
		s = strings.Trim(p.Alias, "\"") + " "
	}
	s += "\"" + strings.Trim(p.Path, "\"") + "\""
	return s
}

type Interface struct {
	Name          string
	SubInterfaces []*Interface
	Methods       []*Method
}

func (i *Interface) AllMethods() []*Method {
	var methods []*Method
	for _, subi := range i.SubInterfaces {
		methods = append(methods, subi.AllMethods()...)
	}
	methods = append(methods, i.Methods...)
	return methods
}

func NewMetaData() *MetaData {
	m := &MetaData{
		pkgs:       map[string]*Pkg{},
		interfaces: map[string]*Interface{},
		stubPkgs:   map[string]bool{},
	}
	return m
}

type MetaData struct {
	file       string
	name       string
	pkgs       map[string]*Pkg
	interfaces map[string]*Interface

	stubPkgs map[string]bool
}

func (meta *MetaData) Name() string {
	return meta.name
}

func (meta *MetaData) Pkgs() map[string]*Pkg {
	return meta.pkgs
}

func (meta *MetaData) StubPkgs() []*Pkg {
	var stubPkgs []*Pkg
	for _, p := range meta.pkgs {
		var name string
		if p.Alias != "" {
			name = p.Alias
		} else {
			arr := strings.Split(p.Path, "/")
			name = arr[len(arr)-1]
		}
		if _, ok := meta.stubPkgs[name]; ok {
			stubPkgs = append(stubPkgs, p)
		}
	}
	return stubPkgs
}

func (meta *MetaData) Interfaces() map[string]*Interface {
	return meta.interfaces
}

func (meta *MetaData) EvalField(field *ast.Field) (ab *ArgBlock) {
	var names []string
	for _, nn := range field.Names {
		names = append(names, nn.Name)
	}
	ab = &ArgBlock{
		Names: names,
		Type:  meta.evaluateExpr(field.Type),
	}
	return
}

func (meta *MetaData) Parse(file string) {
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, file, nil, parser.ParseComments)
	if err != nil {
		log.Fatal(err)
	}
	_, meta.file = path.Split(file)
	meta.name = f.Name.Name
	for _, pkg := range f.Imports {
		var pkgName string
		if pkg.Name != nil {
			pkgName = pkg.Name.Name
		} else {
			arr := strings.Split(pkg.Path.Value, "/")
			pkgName = arr[len(arr)-1]
		}
		meta.AddPkg(&Pkg{
			Alias: pkgName,
			Path:  pkg.Path.Value,
		})
	}
	for _, d := range f.Decls {
		dd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		if dd.Tok != token.TYPE {
			continue
		}
		for _, s := range dd.Specs {
			nn := reflect.TypeOf(s).Elem().Name()
			switch nn {
			case "TypeSpec":
				ss := s.(*ast.TypeSpec)
				sst, ok := ss.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				it := &Interface{}
				it.Name = ss.Name.Name
				for _, m := range sst.Methods.List {
					if m.Names != nil {
						p := m.Type.(*ast.FuncType).Params
						r := m.Type.(*ast.FuncType).Results
						var params, results []*ArgBlock
						if p != nil {
							for _, pp := range p.List {
								params = append(params, meta.EvalField(pp))
							}
						}
						if r != nil {
							for _, rr := range r.List {
								results = append(results, meta.EvalField(rr))
							}
						}
						doc := ""
						comment := ""
						if m.Comment != nil {
							comment = m.Comment.Text()
						}
						if m.Doc != nil {
							doc = m.Doc.Text()
						}
						method := NewMethod(m.Names[0].Name, doc, comment, params, results)
						it.Methods = append(it.Methods, method)
					} else {
						mi := m.Type.(*ast.Ident)
						it.SubInterfaces = append(it.SubInterfaces, meta.GetInterface(mi.Name))
					}
				}
				meta.AddInterface(it)
			}
		}
	}
}

func (meta *MetaData) AddPkg(pkg *Pkg) {
	var pkgName string
	if len(pkg.Alias) != 0 {
		pkg.Alias = strings.Trim(pkg.Alias, "\"")
		pkgName = pkg.Alias
	} else {
		arr := strings.Split(pkg.Path, "/")
		pkgName = arr[len(arr)-1]
	}
	pkg.Path = strings.Trim(pkg.Path, "\"")
	if strings.HasSuffix(pkg.Path, pkg.Alias) {
		pkg.Alias = ""
	}
	meta.pkgs[strings.Trim(pkgName, "\"")] = pkg
}

func (meta *MetaData) AddStubPkg(pkg string) {
	meta.stubPkgs[pkg] = true
}

func (meta *MetaData) AddInterface(i *Interface) {
	meta.interfaces[i.Name] = i
}

func (meta *MetaData) GetInterface(name string) *Interface {
	return meta.interfaces[name]
}

func (meta *MetaData) evaluateExpr(expr ast.Expr) (s string) {
	an := reflect.TypeOf(expr).Elem().Name()
	switch an {
	case "Ident":
		s = expr.(*ast.Ident).Name
	case "InterfaceType":
		s = "interface{}"
	case "StarExpr":
		X := expr.(*ast.StarExpr).X
		if xi, ok := X.(*ast.Ident); ok {
			s = "*" + xi.Name
		} else {
			s = "*" + meta.evaluateExpr(X)
		}
	case "Expr":
		s = expr.(ast.Expr).(*ast.Ident).Name
	case "SelectorExpr":
		ppp := expr.(*ast.SelectorExpr)
		meta.AddStubPkg(ppp.X.(*ast.Ident).Name)
		s = fmt.Sprintf("%s.%s", ppp.X.(*ast.Ident).Name, ppp.Sel.Name)
	case "ArrayType":
		ppp := expr.(*ast.ArrayType)
		s = "[]" + meta.evaluateExpr(ppp.Elt)
	case "MapType":
		ppp := expr.(*ast.MapType)
		s = fmt.Sprintf("map[%s]%s", meta.evaluateExpr(ppp.Key), meta.evaluateExpr(ppp.Value))
	case "Ellipsis":
		ppp := expr.(*ast.Ellipsis)
		s = "..." + meta.evaluateExpr(ppp.Elt)
	default:
		println("unsupported type:", an)
	}
	return
}

type ArgBlock struct {
	Names []string
	Type  string
}

func NewMethod(fname string, doc string, comment string, params []*ArgBlock, results []*ArgBlock) *Method {
	return &Method{
		Name:    fname,
		Doc:     doc,
		Comment: comment,
		Params:  params,
		Results: results,
	}
}

type Method struct {
	Name    string
	Doc     string
	Comment string
	Params  []*ArgBlock
	Results []*ArgBlock
}

func (f *Method) String() string {
	w := bytes.NewBuffer([]byte{})
	w.WriteString(f.Name + "(")
	for i, pb := range f.Params {
		if len(pb.Names) != 0 {
			w.WriteString(strings.Join(pb.Names, ","))
			w.WriteString(" ")
		}
		w.WriteString(pb.Type)
		if i < len(f.Params)-1 {
			w.WriteString(", ")
		}
	}
	w.WriteString(") ")
	if len(f.Results) == 0 {
		return w.String()
	}
	if len(f.Results) == 1 && f.Results[0].Names == nil {
		w.WriteString(f.Results[0].Type)
		return w.String()
	}
	w.WriteString("(")
	for i, rb := range f.Results {
		if len(rb.Names) != 0 {
			w.WriteString(strings.Join(rb.Names, ","))
			w.WriteString(" ")
		}
		w.WriteString(rb.Type)
		if i < len(f.Results)-1 {
			w.WriteString(", ")
		}
	}
	w.WriteString(")")
	return w.String()
}
