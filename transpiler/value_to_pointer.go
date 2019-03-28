package transpiler

import (
	"bytes"
	"fmt"
	goast "go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"html/template"
	"sort"
	"strings"

	"github.com/Konstantin8105/c4go/ast"
	"github.com/Konstantin8105/c4go/program"
	"github.com/Konstantin8105/c4go/types"
	"github.com/Konstantin8105/c4go/util"
)

const unsafeConvertFunctionName string = "c4goUnsafeConvert_"

func ConvertValueToPointer(nodes []ast.Node, p *program.Program) (expr goast.Expr, ok bool) {
	if len(nodes) != 1 {
		return nil, false
	}

	decl, ok := nodes[0].(*ast.DeclRefExpr)
	if !ok {
		return nil, false
	}

	if types.IsPointer(decl.Type, p) {
		return nil, false
	}

	// get base type if it typedef
	var td string = decl.Type
	for {
		if t, ok := p.TypedefType[td]; ok {
			td = t
			continue
		}
		break
	}

	resolvedType, err := types.ResolveType(p, td)
	if err != nil {
		p.AddMessage(p.GenerateWarningMessage(err, decl))
		return
	}

	var acceptable bool

	if types.IsGoBaseType(resolvedType) {
		acceptable = true
	}

	if str, ok := p.Structs[decl.Type]; ok && str.IsGlobal {
		acceptable = true
	}

	if str, ok := p.Unions[decl.Type]; ok && str.IsGlobal {
		acceptable = true
	}

	if !acceptable {
		return nil, false
	}

	// can simplify
	p.UnsafeConvertValueToPointer[resolvedType] = true

	return util.NewCallExpr(fmt.Sprintf("%s%s", unsafeConvertFunctionName,
		typeToFuncname(resolvedType)),
		&goast.UnaryExpr{
			Op: token.AND,
			X:  goast.NewIdent(decl.Name),
		}), true
}

func typeToFuncname(typeName string) (functionName string) {
	functionName = typeName
	if index := strings.Index(functionName, "."); index > -1 {
		functionName = functionName[index+1:]
	}
	return
}

func GetUnsafeConvertDecls(p *program.Program) {
	if len(p.UnsafeConvertValueToPointer) == 0 {
		return
	}

	p.AddImport("unsafe")

	var names []string
	for t := range p.UnsafeConvertValueToPointer {
		names = append(names, t)
	}
	sort.Sort(sort.StringSlice(names))

	for _, t := range names {
		functionName := fmt.Sprintf("%s%s", unsafeConvertFunctionName,
			typeToFuncname(t))
		varName := "c4go_name"
		p.File.Decls = append(p.File.Decls, &goast.FuncDecl{
			Doc: &goast.CommentGroup{
				List: []*goast.Comment{
					&goast.Comment{
						Text: fmt.Sprintf("// %s : created by c4go\n", functionName),
					},
				},
			},
			Name: goast.NewIdent(functionName),
			Type: &goast.FuncType{
				Params: &goast.FieldList{
					List: []*goast.Field{
						{
							Names: []*goast.Ident{goast.NewIdent(varName)},
							Type:  goast.NewIdent("*" + t),
						},
					},
				},
				Results: &goast.FieldList{
					List: []*goast.Field{
						{
							Type: &goast.ArrayType{
								Lbrack: 1,
								Elt:    goast.NewIdent(t),
							},
						},
					},
				},
			},
			Body: &goast.BlockStmt{
				List: []goast.Stmt{
					&goast.ReturnStmt{
						Results: []goast.Expr{
							&goast.SliceExpr{
								X: util.NewCallExpr(fmt.Sprintf("(*[1000000]%s)", t),
									util.NewCallExpr("unsafe.Pointer",
										goast.NewIdent(varName)),
								),
							},
						},
					},
				},
			},
		})
	}

	return
}

// ---------------- POINTER OPERATIONS ---------------------------------------
// Examples:
//	1) pointer + integer - integer
//	2) pointer1 > pointer2
//	3) pointer1 == pointer2
//	4) pointer1 - pointer2
//	5) pointer to integer address
//	6) integer address to pointer
//	7) pointerType1 to pointerType2
//
// Simplification:
//	1) pointer +/- integer
//	2) (pointer1 - pointer2) >  0
//	3) (pointer1 - pointer2) == 0
//	4) (pointer1 - pointer2)
//	5) pointer to integer address
//	6) integer address to pointer
//	7) pointerType1 to integer address to pointerType2
//

// GetPointerAddress - return goast expression with pointer address.
// 		pnt       - goast expression. Foe example: `&a`, `&a[11]`.
//		sizeof    - sizeof of C type.
//		rs        - result goast expression.
//		postStmts - slice of goast.Stmt for runtime.KeepAlive of pointer,
//		            the best way kept that stmts at the end of function.
//		            Each stmt has `defer` functions.
func GetPointerAddress(expr goast.Expr, cType string, sizeof int) (
	rs goast.Expr, postStmts []goast.Stmt, err error) {

	// generate postStmts

	// TODO: runtime.KeepAlive()

	if par, ok := expr.(*goast.ParenExpr); ok {
		// ignore parens
		return GetPointerAddress(par.X, cType, sizeof)
	}

	if id, ok := expr.(*goast.Ident); ok {
		if id.Name == "nil" {
			// nil pointer
			rs = goast.NewIdent("0")
			return
		}
	}

	if _, ok := expr.(*goast.Ident); ok {
		expr = &goast.IndexExpr{
			X:     expr,
			Index: goast.NewIdent("0"),
		}
	}

	if _, ok := expr.(*goast.SelectorExpr); ok {
		expr = &goast.IndexExpr{
			X:     expr,
			Index: goast.NewIdent("0"),
		}
	}

	if sl, ok := expr.(*goast.SliceExpr); ok {
		// from :
		//
		// 88  0: *ast.SliceExpr {
		// 89  .  X: *ast.Ident {
		// 91  .  .  Name: "b"
		// 93  .  }
		// 95  .  Low: *ast.BasicLit { ... }
		// 99  .  }
		// 102  }
		//
		// to:
		//
		// 0  *ast.IndexExpr {
		// 1  .  X: *ast.Ident {
		// 3  .  .  Name: "b"
		// 4  .  }
		// 6  .  Index: *ast.BasicLit { ... }
		// 12  }
		if sl.Low == nil {
			sl.Low = goast.NewIdent("0")
		}
		util.PanicIfNil(sl.X, "slice is nil")
		util.PanicIfNil(sl.Low, "slice low is nil")
		expr = &goast.IndexExpr{
			X:     sl.X,
			Index: sl.Low,
		}
	}

	if sl, ok := expr.(*goast.SliceExpr); ok {
		if c, ok := sl.X.(*goast.CallExpr); ok {
			if fin, ok := c.Fun.(*goast.Ident); ok && strings.Contains(fin.Name, "1000000") {
				if len(c.Args) == 1 {
					if cc, ok := c.Args[0].(*goast.CallExpr); ok {
						if fin, ok := cc.Fun.(*goast.Ident); ok && strings.Contains(fin.Name, "unsafe.Pointer") {
							if len(cc.Args) == 1 {
								if un, ok := cc.Args[0].(*goast.UnaryExpr); ok && un.Op == token.AND {
									expr = un.X
								}
							}
						}
					}
				}
			}
		}
	}

	if _, ok := expr.(*goast.CallExpr); ok {
		name := "c4go_temp_name"
		rs = util.NewAnonymousFunction(
			// body
			[]goast.Stmt{
				&goast.ExprStmt{
					X: &goast.BinaryExpr{
						X:  goast.NewIdent(name),
						Op: token.DEFINE,
						Y:  expr,
					},
				},
			},
			// defer
			nil,
			// returnValue
			util.NewCallExpr("int64", util.NewCallExpr("uintptr", util.NewCallExpr("unsafe.Pointer",
				&goast.StarExpr{
					Star: 1,
					X: &goast.CallExpr{
						Fun:    goast.NewIdent("(**byte)"),
						Lparen: 1,
						Args: []goast.Expr{&goast.CallExpr{
							Fun:    goast.NewIdent("unsafe.Pointer"),
							Lparen: 1,
							Args: []goast.Expr{
								&goast.UnaryExpr{
									Op: token.AND,
									X:  goast.NewIdent(name),
								},
							},
						}},
					},
				},
			))),
			// returnType
			"int64",
		)
		return
	}

	// prepare postStmts

	if sizeof < 1 {
		err = fmt.Errorf("Not valid sizeof `%s`: %d", cType, sizeof)
		return
	}

	// main result expression
	rs = &goast.BinaryExpr{
		X: util.NewCallExpr("int64", util.NewCallExpr("uintptr",
			util.NewCallExpr("unsafe.Pointer",
				&goast.UnaryExpr{Op: token.AND, X: expr},
			),
		)),
		Op: token.QUO,
		Y:  util.NewCallExpr("int64", goast.NewIdent(fmt.Sprintf("%d", sizeof))),
	}

	// return results
	return
}

//	SubTwoPnts function for implementation : (pointer1 - pointer2)
func SubTwoPnts(
	val1 goast.Expr, val1Type string,
	val2 goast.Expr, val2Type string,
	sizeof int) (rs goast.Expr, postStmts []goast.Stmt, err error) {

	x, newPost, err := GetPointerAddress(val1, val1Type, sizeof)
	if err != nil {
		return
	}
	postStmts = append(postStmts, newPost...)

	y, newPost, err := GetPointerAddress(val2, val2Type, sizeof)
	if err != nil {
		return
	}
	postStmts = append(postStmts, newPost...)

	rs = &goast.ParenExpr{X: &goast.BinaryExpr{X: x, Op: token.SUB, Y: y}}

	return
}

//		postStmts - slice of goast.Stmt for runtime.KeepAlive of pointer,
//		            the best way kept that stmts at the end of function.
//		            Each stmt has `defer` functions.
func PntCmpPnt(
	p *program.Program,
	val1 goast.Expr, val1Type string,
	val2 goast.Expr, val2Type string,
	sizeof int, operator token.Token,
) (
	rs goast.Expr,
	postStmts []goast.Stmt,
	err error,
) {

	switch operator {
	case token.SUB: // -
		p.AddImport("unsafe")
		sub, newPost, err := SubTwoPnts(val1, val1Type, val2, val2Type, sizeof)
		postStmts = append(postStmts, newPost...)
		return sub, postStmts, err
	case token.LAND, token.LOR: // && ||
		// TODO: add tests
		p.AddImport("unsafe")
		var newPost []goast.Stmt
		val1, newPost, err = PntCmpPnt(
			p,
			val1, val1Type,
			goast.NewIdent("nil"), types.NullPointer,
			sizeof, token.EQL)
		if err != nil {
			return
		}
		postStmts = append(postStmts, newPost...)
		val2, newPost, err = PntCmpPnt(
			p,
			val2, val2Type,
			goast.NewIdent("nil"), types.NullPointer,
			sizeof, token.EQL)
		if err != nil {
			return
		}
		postStmts = append(postStmts, newPost...)
		rs = &goast.BinaryExpr{
			X:  val1,
			Op: operator,
			Y:  val2,
		}
		return
	}

	// > >= > <= ==

	{
		// specific for operations with nil
		isExprNil := func(node goast.Expr) bool {
			id, ok := node.(*goast.Ident)
			if !ok {
				return false
			}
			if id.Name != "nil" {
				return false
			}
			return true
		}

		if !(isExprNil(val1) && isExprNil(val2)) {
			// Examples:
			// val1 != nil
			// val1 == nil
			// val1  > nil

			ignoreList := func(Type string) bool {
				return util.IsFunction(Type) ||
					Type == types.NullPointer ||
					Type == "void *" ||
					Type == "FILE *"
			}

			switch {
			case isExprNil(val2):
				if !ignoreList(val1Type) {
					val1 = util.NewCallExpr("len", val1)
					val2 = goast.NewIdent("0")
				}
				rs = &goast.BinaryExpr{
					X:  val1,
					Op: operator,
					Y:  val2,
				}
				return

			case isExprNil(val1):
				if !ignoreList(val2Type) {
					val1 = goast.NewIdent("0")
					val2 = util.NewCallExpr("len", val2)
				}
				rs = &goast.BinaryExpr{
					X:  val1,
					Op: operator,
					Y:  val2,
				}
				return
			}
		}
	}

	p.AddImport("unsafe")
	sub, newPost, err := SubTwoPnts(val1, val1Type, val2, val2Type, sizeof)
	postStmts = append(postStmts, newPost...)

	rs = &goast.BinaryExpr{
		X:  sub,
		Op: operator,
		Y:  goast.NewIdent("0"),
	}

	return
}

// PntBitCast - casting pointers
func PntBitCast(expr goast.Expr, cFrom, cTo string, p *program.Program) (
	rs goast.Expr, toCtype string, postStmts []goast.Stmt, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot PntBitCast : %v", err)
			p.AddMessage(p.GenerateWarningMessage(err, nil))
		}
	}()

	cFrom = util.GenerateCorrectType(cFrom)
	cTo = util.GenerateCorrectType(cTo)
	toCtype = cTo

	if !types.IsPointer(cFrom, p) || !types.IsPointer(cTo, p) {
		err = fmt.Errorf("Some type is not pointer `%s` or `%s`", cFrom, cTo)
		return
	}

	rs = expr

	if cFrom == cTo {
		// no need cast
		return
	}

	if util.IsFunction(cFrom) {
		return
	}

	{
		from, errf := types.ResolveType(p, cFrom)
		to, errto := types.ResolveType(p, cTo)
		if from == to && errf == nil && errto == nil {
			// no need cast
			return
		}
	}

	if cTo == "void *" {
		// no need cast
		return
	}

	if cFrom == "void *" {
		// no need cast
		rs, err = types.CastExpr(p, expr, cFrom, cTo)
		return
	}

	rs, postStmts, err = GetPointerAddress(expr, cFrom, 1)
	if err != nil {
		return
	}
	resolvedType, err := types.ResolveType(p, cTo)
	if err != nil {
		return
	}

	resolvedType = strings.Replace(resolvedType, "[]", "[1000000]", 1)

	p.AddImport("unsafe")
	rs = util.NewCallExpr("(*"+resolvedType+")", util.NewCallExpr("unsafe.Pointer",
		util.NewCallExpr("uintptr", rs)))

	rs = &goast.SliceExpr{
		X:      rs,
		Slice3: false,
	}

	return
}

// CreateSliceFromReference - create a slice, like :
// (*[1]int)(unsafe.Pointer(&a))[:]
func CreateSliceFromReference(goType string, expr goast.Expr) goast.Expr {
	// If the Go type is blank it means that the C type is 'void'.
	if goType == "" {
		goType = "interface{}"
	}

	// This is a hack to convert a reference to a variable into a slice that
	// points to the same location. It will look similar to:
	//
	//     (*[1]int)(unsafe.Pointer(&a))[:]
	//
	// You must always call this Go before using CreateSliceFromReference:
	//
	//     p.AddImport("unsafe")
	//
	return &goast.SliceExpr{
		X: util.NewCallExpr(fmt.Sprintf("(*[1000000]%s)", goType),
			util.NewCallExpr("unsafe.Pointer",
				&goast.UnaryExpr{
					X:  expr,
					Op: token.AND,
				}),
		),
	}
}

// pointerArithmetic - operations between 'int' and pointer
// Example C code : ptr += i
// ptr = (*(*[1]int)(unsafe.Pointer(uintptr(unsafe.Pointer(&ptr[0])) + (i)*unsafe.Sizeof(ptr[0]))))[:]
// , where i  - right
//        '+' - operator
//      'ptr' - left
//      'int' - leftType transpiled in Go type
// Note:
// 1) rigthType MUST be 'int'
// 2) pointerArithmetic - implemented ONLY right part of formula
// 3) right is MUST be positive value, because impossible multiply uintptr to (-1)
func pointerArithmetic(p *program.Program,
	left goast.Expr, leftType string,
	right goast.Expr, rightType string,
	operator token.Token) (
	_ goast.Expr, _ string, preStmts []goast.Stmt, postStmts []goast.Stmt, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Cannot transpile pointerArithmetic. err = %v", err)
		}
	}()

	if bl, ok := right.(*goast.BasicLit); ok && operator == token.ADD {
		if bl.Value == "1" && bl.Kind == token.INT {
			return &goast.SliceExpr{
				X:      left,
				Lbrack: 1,
				Low: &goast.BasicLit{
					Kind:  token.INT,
					Value: "1",
				},
				Slice3: false,
			}, leftType, preStmts, postStmts, nil
		}
	}

	if !(types.IsCInteger(p, rightType) || rightType == "bool") {
		err = fmt.Errorf("right type is not C integer type : '%s'", rightType)
		return
	}
	if !types.IsPointer(leftType, p) {
		err = fmt.Errorf("left type is not a pointer : '%s'", leftType)
		return
	}
	right, err = types.CastExpr(p, right, rightType, "int")
	if err != nil {
		return
	}

	for {
		if t, ok := p.TypedefType[leftType]; ok {
			leftType = t
			continue
		}
		break
	}

	resolvedLeftType, err := types.ResolveType(p, leftType)
	if err != nil {
		return
	}

	type pA struct {
		Name      string // name of variable: 'ptr'
		Type      string // type of variable: 'int','double'
		Condition string // condition : '-1' ,'(-1+2-2)'
		Operator  string // operator : '+', '-'
	}

	var s pA

	{
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, token.NewFileSet(), left)
		s.Name = buf.String()
	}
	{
		var buf bytes.Buffer
		_ = printer.Fprint(&buf, token.NewFileSet(), right)
		s.Condition = buf.String()
	}

	s.Type = resolvedLeftType
	if resolvedLeftType != "interface{}" {
		s.Type = resolvedLeftType[2:]
	}

	s.Operator = "+"
	if operator == token.SUB {
		s.Operator = "-"
	}

	src := `package main
func main(){
	a := (*(*[1000000]{{ .Type }})(unsafe.Pointer(uintptr(
			unsafe.Pointer(&{{ .Name }}[0])) {{ .Operator }}
			(uintptr)({{ .Condition }})*unsafe.Sizeof({{ .Name }}[0]))))[:]
}`
	tmpl := template.Must(template.New("").Parse(src))
	var source bytes.Buffer
	err = tmpl.Execute(&source, s)
	if err != nil {
		err = fmt.Errorf("Cannot execute template. err = %v", err)
		return
	}

	// Create the AST by parsing src.
	fset := token.NewFileSet() // positions are relative to fset
	body := strings.Replace(source.String(), "&#43;", "+", -1)
	body = strings.Replace(body, "&amp;", "&", -1)
	body = strings.Replace(body, "&#34;", "\"", -1)
	body = strings.Replace(body, "&#39;", "'", -1)
	body = strings.Replace(body, "&gt;", ">", -1)
	body = strings.Replace(body, "&lt;", "<", -1)
	// TODO: add unicode convertor
	f, err := parser.ParseFile(fset, "", body, 0)
	if err != nil {
		body = strings.Replace(body, "\n", "", -1)
		err = fmt.Errorf("Cannot parse file. err = %v. body = `%s`", err, body)
		return
	}

	p.AddImport("unsafe")

	return f.Decls[0].(*goast.FuncDecl).Body.List[0].(*goast.AssignStmt).Rhs[0],
		leftType, preStmts, postStmts, nil
}