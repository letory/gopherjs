package translator

import (
	"code.google.com/p/go.tools/go/exact"
	"code.google.com/p/go.tools/go/types"
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

func (c *PkgContext) translateExpr(expr ast.Expr) string {
	exprType := c.info.Types[expr]
	if value, valueFound := c.info.Values[expr]; valueFound {
		basic := types.Typ[types.String]
		if value.Kind() != exact.String { // workaround for bug in go/types
			basic = exprType.Underlying().(*types.Basic)
		}
		switch {
		case basic.Info()&types.IsBoolean != 0:
			return strconv.FormatBool(exact.BoolVal(value))
		case basic.Info()&types.IsInteger != 0:
			if is64Bit(basic) {
				d, _ := exact.Uint64Val(value)
				return fmt.Sprintf("new %s(%d, %d)", c.typeName(exprType), d>>32, d&(1<<32-1))
			}
			d, _ := exact.Int64Val(value)
			return strconv.FormatInt(d, 10)
		case basic.Info()&types.IsFloat != 0:
			f, _ := exact.Float64Val(value)
			return strconv.FormatFloat(f, 'g', -1, 64)
		case basic.Info()&types.IsComplex != 0:
			r, _ := exact.Float64Val(exact.Real(value))
			i, _ := exact.Float64Val(exact.Imag(value))
			if basic.Kind() == types.UntypedComplex {
				exprType = types.Typ[types.Complex128]
			}
			return fmt.Sprintf("new %s(%s, %s)", c.typeName(exprType), strconv.FormatFloat(r, 'g', -1, 64), strconv.FormatFloat(i, 'g', -1, 64))
		case basic.Info()&types.IsString != 0:
			return encodeString(exact.StringVal(value))
		default:
			panic("Unhandled constant type: " + basic.String())
		}
	}

	switch e := expr.(type) {
	case *ast.CompositeLit:
		if ptrType, isPointer := exprType.(*types.Pointer); isPointer {
			exprType = ptrType.Elem()
		}

		collectIndexedElements := func(elementType types.Type) []string {
			elements := make([]string, 0)
			i := 0
			zero := c.zeroValue(elementType)
			for _, element := range e.Elts {
				if kve, isKve := element.(*ast.KeyValueExpr); isKve {
					key, _ := exact.Int64Val(c.info.Values[kve.Key])
					i = int(key)
					element = kve.Value
				}
				for len(elements) <= i {
					elements = append(elements, zero)
				}
				elements[i] = c.translateExprToType(element, elementType)
				i++
			}
			return elements
		}

		switch t := exprType.Underlying().(type) {
		case *types.Array:
			elements := collectIndexedElements(t.Elem())
			if len(elements) != 0 {
				zero := c.zeroValue(t.Elem())
				for len(elements) < int(t.Len()) {
					elements = append(elements, zero)
				}
				return fmt.Sprintf(`go$toNativeArray("%s", [%s])`, typeKind(t.Elem()), strings.Join(elements, ", "))
			}
			return fmt.Sprintf(`go$makeNativeArray("%s", %d, function() { return %s; })`, typeKind(t.Elem()), t.Len(), c.zeroValue(t.Elem()))
		case *types.Slice:
			return fmt.Sprintf("new %s([%s])", c.typeName(exprType), strings.Join(collectIndexedElements(t.Elem()), ", "))
		case *types.Map:
			mapVar := c.newVariable("_map")
			keyVar := c.newVariable("_map")
			assignments := ""
			for _, element := range e.Elts {
				kve := element.(*ast.KeyValueExpr)
				assignments += fmt.Sprintf(`%s = %s, %s[%s] = { k: %s, v: %s }, `, keyVar, c.translateExprToType(kve.Key, t.Key()), mapVar, c.makeKey(c.newIdent(keyVar, t.Key()), t.Key()), keyVar, c.translateExprToType(kve.Value, t.Elem()))
			}
			return fmt.Sprintf("(%s = new Go$Map(), %s%s)", mapVar, assignments, mapVar)
		case *types.Struct:
			elements := make([]string, t.NumFields())
			isKeyValue := true
			if len(e.Elts) != 0 {
				_, isKeyValue = e.Elts[0].(*ast.KeyValueExpr)
			}
			if !isKeyValue {
				for i, element := range e.Elts {
					elements[i] = c.translateExprToType(element, t.Field(i).Type())
				}
			}
			if isKeyValue {
				for i := range elements {
					elements[i] = c.zeroValue(t.Field(i).Type())
				}
				for _, element := range e.Elts {
					kve := element.(*ast.KeyValueExpr)
					for j := range elements {
						if kve.Key.(*ast.Ident).Name == t.Field(j).Name() {
							elements[j] = c.translateExprToType(kve.Value, t.Field(j).Type())
							break
						}
					}
				}
			}
			return fmt.Sprintf("new %s.Ptr(%s)", c.typeName(exprType), strings.Join(elements, ", "))
		default:
			panic(fmt.Sprintf("Unhandled CompositeLit type: %T\n", t))
		}

	case *ast.FuncLit:
		return strings.TrimSpace(string(c.CatchOutput(func() {
			c.newScope(func() {
				params := c.translateParams(e.Type)
				closurePrefix := "("
				closureSuffix := ")"
				if len(c.escapingVars) != 0 {
					list := strings.Join(c.escapingVars, ", ")
					closurePrefix = "(function(" + list + ") { return "
					closureSuffix = "; })(" + list + ")"
				}
				c.Printf("%sfunction(%s) {", closurePrefix, strings.Join(params, ", "))
				c.Indent(func() {
					c.translateFunctionBody(e.Body.List, exprType.(*types.Signature))
				})
				c.Printf("}%s", closureSuffix)
			})
		})))

	case *ast.UnaryExpr:
		switch e.Op {
		case token.AND:
			switch c.info.Types[e.X].Underlying().(type) {
			case *types.Struct, *types.Array:
				return c.translateExpr(e.X)
			default:
				if _, isComposite := e.X.(*ast.CompositeLit); isComposite {
					return fmt.Sprintf("go$newDataPointer(%s, %s)", c.translateExpr(e.X), c.typeName(c.info.Types[e]))
				}
				closurePrefix := ""
				closureSuffix := ""
				if len(c.escapingVars) != 0 {
					list := strings.Join(c.escapingVars, ", ")
					closurePrefix = "(function(" + list + ") { return "
					closureSuffix = "; })(" + list + ")"
				}
				vVar := c.newVariable("v")
				return fmt.Sprintf("%snew %s(function() { return %s; }, function(%s) { %s; })%s", closurePrefix, c.typeName(exprType), c.translateExpr(e.X), vVar, c.translateAssign(e.X, vVar), closureSuffix)
			}
		case token.ARROW:
			return "undefined"
		}

		t := c.info.Types[e.X]
		basic := t.Underlying().(*types.Basic)
		op := e.Op.String()
		switch e.Op {
		case token.ADD:
			return c.translateExpr(e.X)
		case token.SUB:
			if is64Bit(basic) {
				x := c.newVariable("x")
				return fmt.Sprintf("(%s = %s, new %s(-%s.high, -%s.low))", x, c.translateExpr(e.X), c.typeName(t), x, x)
			}
			if basic.Info()&types.IsComplex != 0 {
				x := c.newVariable("x")
				return fmt.Sprintf("(%s = %s, new %s(-%s.real, -%s.imag))", x, c.translateExpr(e.X), c.typeName(t), x, x)
			}
		case token.XOR:
			if is64Bit(basic) {
				x := c.newVariable("x")
				return fmt.Sprintf("(%s = %s, new %s(~%s.high, ~%s.low >>> 0))", x, c.translateExpr(e.X), c.typeName(t), x, x)
			}
			op = "~"
		case token.NOT:
			x := c.translateExpr(e.X)
			if x == "true" {
				return "false"
			}
			if x == "false" {
				return "true"
			}
			return "!" + x
		}
		return fixNumber(fmt.Sprintf("%s%s", op, c.translateExpr(e.X)), basic)

	case *ast.BinaryExpr:
		if e.Op == token.NEQ {
			return fmt.Sprintf("!(%s)", c.translateExpr(&ast.BinaryExpr{
				X:  e.X,
				Op: token.EQL,
				Y:  e.Y,
			}))
		}

		t := c.info.Types[e.X]
		t2 := c.info.Types[e.Y]
		_, isInterface := t2.Underlying().(*types.Interface)
		if isInterface {
			t = t2
		}

		if basic, isBasic := t.Underlying().(*types.Basic); isBasic && basic.Info()&types.IsNumeric != 0 {
			if is64Bit(basic) {
				var expr string
				switch e.Op {
				case token.MUL:
					return fmt.Sprintf("go$mul64(%s, %s)", c.translateExpr(e.X), c.translateExpr(e.Y))
				case token.QUO:
					return fmt.Sprintf("go$div64(%s, %s, false)", c.translateExpr(e.X), c.translateExpr(e.Y))
				case token.REM:
					return fmt.Sprintf("go$div64(%s, %s, true)", c.translateExpr(e.X), c.translateExpr(e.Y))
				case token.SHL:
					return fmt.Sprintf("go$shiftLeft64(%s, %s)", c.translateExpr(e.X), c.flatten64(e.Y))
				case token.SHR:
					return fmt.Sprintf("go$shiftRight%s(%s, %s)", toJavaScriptType(basic), c.translateExpr(e.X), c.flatten64(e.Y))
				case token.EQL:
					expr = "x.high === y.high && x.low === y.low"
				case token.LSS:
					expr = "x.high < y.high || (x.high === y.high && x.low < y.low)"
				case token.LEQ:
					expr = "x.high < y.high || (x.high === y.high && x.low <= y.low)"
				case token.GTR:
					expr = "x.high > y.high || (x.high === y.high && x.low > y.low)"
				case token.GEQ:
					expr = "x.high > y.high || (x.high === y.high && x.low >= y.low)"
				case token.ADD, token.SUB:
					expr = fmt.Sprintf("new %s(x.high %s y.high, x.low %s y.low)", c.typeName(t), e.Op, e.Op)
				case token.AND, token.OR, token.XOR:
					expr = fmt.Sprintf("new %s(x.high %s y.high, (x.low %s y.low) >>> 0)", c.typeName(t), e.Op, e.Op)
				case token.AND_NOT:
					expr = fmt.Sprintf("new %s(x.high &~ y.high, (x.low &~ y.low) >>> 0)", c.typeName(t))
				default:
					panic(e.Op)
				}
				x := c.newVariable("x")
				y := c.newVariable("y")
				expr = strings.Replace(expr, "x.", x+".", -1)
				expr = strings.Replace(expr, "y.", y+".", -1)
				return fmt.Sprintf("(%s = %s, %s = %s, %s)", x, c.translateExpr(e.X), y, c.translateExpr(e.Y), expr)
			}

			if basic.Info()&types.IsComplex != 0 {
				var expr string
				switch e.Op {
				case token.EQL:
					expr = "x.real === y.real && x.imag === y.imag"
				case token.ADD, token.SUB:
					expr = fmt.Sprintf("new %s(x.real %s y.real, x.imag %s y.imag)", c.typeName(t), e.Op, e.Op)
				case token.MUL:
					expr = fmt.Sprintf("new %s(x.real * y.real - x.imag * y.imag, x.real * y.imag + x.imag * y.real)", c.typeName(t))
				case token.QUO:
					return fmt.Sprintf("go$divComplex(%s, %s)", c.translateExpr(e.X), c.translateExpr(e.Y))
				default:
					panic(e.Op)
				}
				x := c.newVariable("x")
				y := c.newVariable("y")
				expr = strings.Replace(expr, "x.", x+".", -1)
				expr = strings.Replace(expr, "y.", y+".", -1)
				return fmt.Sprintf("(%s = %s, %s = %s, %s)", x, c.translateExpr(e.X), y, c.translateExpr(e.Y), expr)
			}

			switch e.Op {
			case token.EQL:
				return fmt.Sprintf("%s === %s", c.translateExpr(e.X), c.translateExpr(e.Y))
			case token.LSS, token.LEQ, token.GTR, token.GEQ:
				return fmt.Sprintf("%s %s %s", c.translateExpr(e.X), e.Op, c.translateExpr(e.Y))
			case token.ADD, token.SUB:
				return fixNumber(fmt.Sprintf("%s %s %s", c.translateExpr(e.X), e.Op, c.translateExpr(e.Y)), basic)
			case token.MUL:
				if basic.Kind() == types.Int32 {
					x := c.newVariable("x")
					y := c.newVariable("y")
					return fmt.Sprintf("(%s = %s, %s = %s, (((%s >>> 16 << 16) * %s >> 0) + (%s << 16 >>> 16) * %s) >> 0)", x, c.translateExpr(e.X), y, c.translateExpr(e.Y), x, y, x, y)
				}
				if basic.Kind() == types.Uint32 || basic.Kind() == types.Uintptr {
					x := c.newVariable("x")
					y := c.newVariable("y")
					return fmt.Sprintf("(%s = %s, %s = %s, (((%s >>> 16 << 16) * %s >>> 0) + (%s << 16 >>> 16) * %s) >>> 0)", x, c.translateExpr(e.X), y, c.translateExpr(e.Y), x, y, x, y)
				}
				return fixNumber(fmt.Sprintf("%s * %s", c.translateExpr(e.X), c.translateExpr(e.Y)), basic)
			case token.QUO:
				value := fmt.Sprintf("%s / %s", c.translateExpr(e.X), c.translateExpr(e.Y))
				if basic.Info()&types.IsInteger != 0 {
					value = "(go$obj = " + value + `, (go$obj === go$obj && go$obj !== 1/0 && go$obj !== -1/0) ? go$obj : go$throwRuntimeError("integer divide by zero"))`
				}
				switch basic.Kind() {
				case types.Int, types.Uint:
					return "(" + value + " >> 0)" // cut off decimals
				default:
					return fixNumber(value, basic)
				}
			case token.REM:
				return fmt.Sprintf(`(go$obj = %s %% %s, go$obj === go$obj ? go$obj : go$throwRuntimeError("integer divide by zero"))`, c.translateExpr(e.X), c.translateExpr(e.Y))
			case token.SHL, token.SHR:
				op := e.Op.String()
				if e.Op == token.SHR && basic.Info()&types.IsUnsigned != 0 {
					op = ">>>"
				}
				if c.info.Values[e.Y] != nil {
					return fixNumber(fmt.Sprintf("%s %s %s", c.translateExpr(e.X), op, c.translateExpr(e.Y)), basic)
				}
				if e.Op == token.SHR && basic.Info()&types.IsUnsigned == 0 {
					return fixNumber(fmt.Sprintf("(%s >> go$min(%s, 31))", c.translateExpr(e.X), c.translateExpr(e.Y)), basic)
				}
				y := c.newVariable("y")
				return fixNumber(fmt.Sprintf("(%s = %s, %s < 32 ? (%s %s %s) : 0)", y, c.translateExprToType(e.Y, types.Typ[types.Uint]), y, c.translateExpr(e.X), op, y), basic)
			case token.AND, token.OR, token.XOR:
				return fixNumber(fmt.Sprintf("(%s %s %s)", c.translateExpr(e.X), e.Op, c.translateExpr(e.Y)), basic)
			case token.AND_NOT:
				return fixNumber(fmt.Sprintf("(%s &~ %s)", c.translateExpr(e.X), c.translateExpr(e.Y)), basic)
			default:
				panic(e.Op)
			}
		}

		switch e.Op {
		case token.ADD, token.LSS, token.LEQ, token.GTR, token.GEQ:
			return fmt.Sprintf("%s %s %s", c.translateExpr(e.X), e.Op, c.translateExpr(e.Y))
		case token.LAND:
			x := c.translateExpr(e.X)
			y := c.translateExpr(e.Y)
			if x == "false" {
				return "false"
			}
			return x + " && " + y
		case token.LOR:
			x := c.translateExpr(e.X)
			y := c.translateExpr(e.Y)
			if x == "true" {
				return "true"
			}
			return x + " || " + y
		case token.EQL:
			switch u := t.Underlying().(type) {
			case *types.Struct:
				x := c.newVariable("x")
				y := c.newVariable("y")
				var conds []string
				for i := 0; i < u.NumFields(); i++ {
					field := u.Field(i)
					if field.Name() == "_" {
						continue
					}
					conds = append(conds, c.translateExpr(&ast.BinaryExpr{
						X:  c.newIdent(x+"."+fieldName(u, i), field.Type()),
						Op: token.EQL,
						Y:  c.newIdent(y+"."+fieldName(u, i), field.Type()),
					}))
				}
				if len(conds) == 0 {
					conds = []string{"true"}
				}
				return fmt.Sprintf("(%s = %s, %s = %s, %s)", x, c.translateExpr(e.X), y, c.translateExpr(e.Y), strings.Join(conds, " && "))
			case *types.Interface:
				return fmt.Sprintf("go$interfaceIsEqual(%s, %s)", c.translateExprToType(e.X, t), c.translateExprToType(e.Y, t))
			case *types.Array:
				return fmt.Sprintf("go$arrayIsEqual(%s, %s)", c.translateExpr(e.X), c.translateExpr(e.Y))
			case *types.Pointer:
				xUnary, xIsUnary := e.X.(*ast.UnaryExpr)
				yUnary, yIsUnary := e.Y.(*ast.UnaryExpr)
				if xIsUnary && xUnary.Op == token.AND && yIsUnary && yUnary.Op == token.AND {
					xIndex, xIsIndex := xUnary.X.(*ast.IndexExpr)
					yIndex, yIsIndex := yUnary.X.(*ast.IndexExpr)
					if xIsIndex && yIsIndex {
						return fmt.Sprintf("go$sliceIsEqual(%s, %s, %s, %s)", c.translateExpr(xIndex.X), c.flatten64(xIndex.Index), c.translateExpr(yIndex.X), c.flatten64(yIndex.Index))
					}
				}
				switch u.Elem().Underlying().(type) {
				case *types.Struct, *types.Interface:
					return c.translateExprToType(e.X, t) + " === " + c.translateExprToType(e.Y, t)
				case *types.Array:
					return fmt.Sprintf("go$arrayIsEqual(%s, %s)", c.translateExprToType(e.X, t), c.translateExprToType(e.Y, t))
				default:
					return fmt.Sprintf("go$pointerIsEqual(%s, %s)", c.translateExprToType(e.X, t), c.translateExprToType(e.Y, t))
				}
			default:
				return c.translateExprToType(e.X, t) + " === " + c.translateExprToType(e.Y, t)
			}
		default:
			panic(e.Op)
		}

	case *ast.ParenExpr:
		x := c.translateExpr(e.X)
		if x == "true" || x == "false" {
			return x
		}
		return "(" + x + ")"

	case *ast.IndexExpr:
		switch t := c.info.Types[e.X].Underlying().(type) {
		case *types.Array, *types.Pointer:
			return fmt.Sprintf("%s[%s]", c.translateExpr(e.X), c.flatten64(e.Index))
		case *types.Slice:
			sliceVar := c.newVariable("_slice")
			indexVar := c.newVariable("_index")
			return fmt.Sprintf(`(%s = %s, %s = %s, (%s >= 0 && %s < %s.length) ? %s.array[%s.offset + %s] : go$throwRuntimeError("index out of range"))`, sliceVar, c.translateExpr(e.X), indexVar, c.flatten64(e.Index), indexVar, indexVar, sliceVar, sliceVar, sliceVar, indexVar)
		case *types.Map:
			key := c.makeKey(e.Index, t.Key())
			if _, isTuple := exprType.(*types.Tuple); isTuple {
				return fmt.Sprintf(`(go$obj = %s[%s], go$obj !== undefined ? [go$obj.v, true] : [%s, false])`, c.translateExpr(e.X), key, c.zeroValue(t.Elem()))
			}
			return fmt.Sprintf(`(go$obj = %s[%s], go$obj !== undefined ? go$obj.v : %s)`, c.translateExpr(e.X), key, c.zeroValue(t.Elem()))
		case *types.Basic:
			return fmt.Sprintf("%s.charCodeAt(%s)", c.translateExpr(e.X), c.flatten64(e.Index))
		default:
			panic(fmt.Sprintf("Unhandled IndexExpr: %T\n", t))
		}

	case *ast.SliceExpr:
		b, isBasic := c.info.Types[e.X].Underlying().(*types.Basic)
		isString := isBasic && b.Info()&types.IsString != 0
		slice := c.translateExprToType(e.X, exprType)
		if e.High == nil {
			if e.Low == nil {
				return slice
			}
			if isString {
				return fmt.Sprintf("%s.substring(%s)", slice, c.flatten64(e.Low))
			}
			return fmt.Sprintf("go$subslice(%s, %s)", slice, c.flatten64(e.Low))
		}
		low := "0"
		if e.Low != nil {
			low = c.flatten64(e.Low)
		}
		if isString {
			return fmt.Sprintf("%s.substring(%s, %s)", slice, low, c.flatten64(e.High))
		}
		if e.Max != nil {
			return fmt.Sprintf("go$subslice(%s, %s, %s, %s)", slice, low, c.flatten64(e.High), c.flatten64(e.Max))
		}
		return fmt.Sprintf("go$subslice(%s, %s, %s)", slice, low, c.flatten64(e.High))

	case *ast.SelectorExpr:
		sel := c.info.Selections[e]
		parameterName := func(v *types.Var) string {
			if v.Anonymous() || v.Name() == "" {
				return c.newVariable("param")
			}
			return c.newVariable(v.Name())
		}
		makeParametersList := func() []string {
			params := sel.Obj().Type().(*types.Signature).Params()
			names := make([]string, params.Len())
			for i := 0; i < params.Len(); i++ {
				names[i] = parameterName(params.At(i))
			}
			return names
		}

		switch sel.Kind() {
		case types.FieldVal:
			return c.translateExpr(e.X) + "." + translateSelection(sel)
		case types.MethodVal:
			parameters := makeParametersList()
			target := c.translateExpr(e.X)
			if isWrapped(sel.Recv()) {
				target = fmt.Sprintf("(new %s(%s))", c.typeName(sel.Recv()), target)
			}
			recv := c.newVariable("_recv")
			return fmt.Sprintf("(%s = %s, function(%s) { return %s.%s(%s); })", recv, target, strings.Join(parameters, ", "), recv, e.Sel.Name, strings.Join(parameters, ", "))
		case types.MethodExpr:
			recv := "recv"
			if isWrapped(sel.Recv()) {
				recv = fmt.Sprintf("(new %s(recv))", c.typeName(sel.Recv()))
			}
			parameters := makeParametersList()
			return fmt.Sprintf("(function(%s) { return %s.%s(%s); })", strings.Join(append([]string{"recv"}, parameters...), ", "), recv, sel.Obj().(*types.Func).Name(), strings.Join(parameters, ", "))
		case types.PackageObj:
			return fmt.Sprintf("%s.%s", c.translateExpr(e.X), e.Sel.Name)
		}
		panic("")

	case *ast.CallExpr:
		plainFun := e.Fun
		for {
			if p, isParen := plainFun.(*ast.ParenExpr); isParen {
				plainFun = p.X
				continue
			}
			break
		}

		switch f := plainFun.(type) {
		case *ast.Ident:
			switch o := c.info.Objects[f].(type) {
			case *types.Builtin:
				switch o.Name() {
				case "new":
					t := c.info.Types[e].(*types.Pointer)
					if types.IsIdentical(t.Elem().Underlying(), types.Typ[types.Uintptr]) {
						return "new Uint8Array(8)"
					}
					switch t.Elem().Underlying().(type) {
					case *types.Struct, *types.Array:
						return c.zeroValue(t.Elem())
					default:
						return fmt.Sprintf("go$newDataPointer(%s, %s)", c.zeroValue(t.Elem()), c.typeName(t))
					}
				case "make":
					switch argType := c.info.Types[e.Args[0]].Underlying().(type) {
					case *types.Slice:
						length := c.translateExprToType(e.Args[1], types.Typ[types.Int])
						capacity := "0"
						if len(e.Args) == 3 {
							capacity = c.translateExprToType(e.Args[2], types.Typ[types.Int])
						}
						return fmt.Sprintf("%s.make(%s, %s, function() { return %s; })", c.typeName(c.info.Types[e.Args[0]]), length, capacity, c.zeroValue(argType.Elem()))
					case *types.Map:
						return "new Go$Map()"
					case *types.Chan:
						return fmt.Sprintf("new %s()", c.typeName(c.info.Types[e.Args[0]]))
					default:
						panic(fmt.Sprintf("Unhandled make type: %T\n", argType))
					}
				case "len":
					arg := c.translateExpr(e.Args[0])
					switch argType := c.info.Types[e.Args[0]].Underlying().(type) {
					case *types.Basic, *types.Slice:
						return arg + ".length"
					case *types.Pointer:
						return fmt.Sprintf("(%s, %d)", arg, argType.Elem().(*types.Array).Len())
					case *types.Map:
						return fmt.Sprintf("go$keys(%s).length", arg)
					case *types.Chan:
						return "0"
					// length of array is constant
					default:
						panic(fmt.Sprintf("Unhandled len type: %T\n", argType))
					}
				case "cap":
					arg := c.translateExpr(e.Args[0])
					switch argType := c.info.Types[e.Args[0]].Underlying().(type) {
					case *types.Slice:
						return arg + ".capacity"
					case *types.Pointer:
						return fmt.Sprintf("(%s, %d)", arg, argType.Elem().(*types.Array).Len())
					case *types.Chan:
						return "0"
					// capacity of array is constant
					default:
						panic(fmt.Sprintf("Unhandled cap type: %T\n", argType))
					}
				case "panic":
					return fmt.Sprintf("throw go$panic(%s)", c.translateExprToType(e.Args[0], types.NewInterface(nil, nil)))
				case "append":
					if e.Ellipsis.IsValid() {
						return fmt.Sprintf("go$append(%s, %s)", c.translateExpr(e.Args[0]), c.translateExprToType(e.Args[1], exprType))
					}
					sliceType := exprType.Underlying().(*types.Slice)
					return fmt.Sprintf("go$append(%s, new %s([%s]))", c.translateExpr(e.Args[0]), c.typeName(exprType), strings.Join(c.translateExprSlice(e.Args[1:], sliceType.Elem()), ", "))
				case "delete":
					return fmt.Sprintf(`delete %s[%s]`, c.translateExpr(e.Args[0]), c.makeKey(e.Args[1], c.info.Types[e.Args[0]].Underlying().(*types.Map).Key()))
				case "copy":
					if basic, isBasic := c.info.Types[e.Args[1]].Underlying().(*types.Basic); isBasic && basic.Info()&types.IsString != 0 {
						return fmt.Sprintf("go$copyString(%s, %s)", c.translateExpr(e.Args[0]), c.translateExpr(e.Args[1]))
					}
					return fmt.Sprintf("go$copySlice(%s, %s)", c.translateExpr(e.Args[0]), c.translateExpr(e.Args[1]))
				case "print", "println":
					return fmt.Sprintf("console.log(%s)", strings.Join(c.translateExprSlice(e.Args, nil), ", "))
				case "complex":
					return fmt.Sprintf("new %s(%s, %s)", c.typeName(c.info.Types[e]), c.translateExpr(e.Args[0]), c.translateExpr(e.Args[1]))
				case "real":
					return c.translateExpr(e.Args[0]) + ".real"
				case "imag":
					return c.translateExpr(e.Args[0]) + ".imag"
				case "recover":
					return "go$recover()"
				case "close":
					return `go$notSupported("close")`
				default:
					panic(fmt.Sprintf("Unhandled builtin: %s\n", o.Name()))
				}
			case *types.TypeName: // conversion
				argType := c.info.Types[e.Args[0]]
				if basic, isBasic := o.Type().Underlying().(*types.Basic); isBasic && !types.IsIdentical(argType, types.Typ[types.UnsafePointer]) {
					if basic.Kind() == types.Uint {
						return "(" + c.translateExprToType(e.Args[0], o.Type()) + " >>> 0)"
					}
					return fixNumber(c.translateExprToType(e.Args[0], o.Type()), basic)
				}
				return c.translateExprToType(e.Args[0], o.Type())
			}
		case *ast.FuncType: // conversion
			return c.translateExprToType(e.Args[0], c.info.Types[plainFun])
		}

		funType := c.info.Types[plainFun]
		sig, isSig := funType.Underlying().(*types.Signature)
		if !isSig { // conversion
			if c.pkg.Path() == "reflect" {
				if call, isCall := e.Args[0].(*ast.CallExpr); isCall && types.IsIdentical(c.info.Types[call.Fun], types.Typ[types.UnsafePointer]) {
					if named, isNamed := funType.(*types.Pointer).Elem().(*types.Named); isNamed {
						return c.translateExpr(call.Args[0]) + "." + named.Obj().Name() // unsafe conversion
					}
				}
			}
			return c.translateExprToType(e.Args[0], funType)
		}

		var fun string
		switch f := plainFun.(type) {
		case *ast.SelectorExpr:
			sel := c.info.Selections[f]

			switch sel.Kind() {
			case types.MethodVal:
				methodName := f.Sel.Name
				if ReservedKeywords[methodName] {
					methodName += "$"
				}
				methodsRecvType := sel.Obj().(*types.Func).Type().(*types.Signature).Recv().Type()
				_, pointerExpected := methodsRecvType.(*types.Pointer)
				_, isPointer := sel.Recv().Underlying().(*types.Pointer)
				_, isStruct := sel.Recv().Underlying().(*types.Struct)
				_, isArray := sel.Recv().Underlying().(*types.Array)
				if pointerExpected && !isPointer && !isStruct && !isArray {
					target := c.translateExpr(f.X)
					vVar := c.newVariable("v")
					fun = fmt.Sprintf("(new %s(function() { return %s; }, function(%s) { %s = %s; })).%s", c.typeName(methodsRecvType), target, vVar, target, vVar, methodName)
					break
				}
				if isWrapped(sel.Recv()) {
					fun = fmt.Sprintf("(new %s(%s)).%s", c.typeName(sel.Recv()), c.translateExpr(f.X), methodName)
					break
				}
				fun = fmt.Sprintf("%s.%s", c.translateExpr(f.X), methodName)
			case types.FieldVal, types.MethodExpr, types.PackageObj:
				fun = c.translateExpr(f)
			default:
				panic("")
			}
		default:
			fun = c.translateExpr(plainFun)
		}
		if len(e.Args) == 1 {
			if tuple, isTuple := c.info.Types[e.Args[0]].(*types.Tuple); isTuple {
				args := make([]ast.Expr, tuple.Len())
				for i := range args {
					args[i] = c.newIdent(fmt.Sprintf("go$tuple[%d]", i), tuple.At(i).Type())
				}
				return fmt.Sprintf("(go$tuple = %s, %s(%s))", c.translateExpr(e.Args[0]), fun, c.translateArgs(sig, args, false))
			}
		}
		return fmt.Sprintf("%s(%s)", fun, c.translateArgs(sig, e.Args, e.Ellipsis.IsValid()))

	case *ast.StarExpr:
		if c1, isCall := e.X.(*ast.CallExpr); isCall && len(c1.Args) == 1 {
			if c2, isCall := c1.Args[0].(*ast.CallExpr); isCall && len(c2.Args) == 1 && types.IsIdentical(c.info.Types[c2.Fun], types.Typ[types.UnsafePointer]) {
				if unary, isUnary := c2.Args[0].(*ast.UnaryExpr); isUnary && unary.Op == token.AND {
					return c.translateExpr(unary.X) // unsafe conversion
				}
			}
		}
		switch exprType.Underlying().(type) {
		case *types.Struct, *types.Array:
			return c.translateExpr(e.X)
		}
		return c.translateExpr(e.X) + ".go$get()"

	case *ast.TypeAssertExpr:
		if e.Type == nil {
			return c.translateExpr(e.X)
		}
		t := c.info.Types[e.Type]
		check := "go$obj !== null && " + c.typeCheck("go$obj.constructor", t)
		value := "go$obj"
		if _, isInterface := t.Underlying().(*types.Interface); !isInterface {
			value += ".go$val"
		}
		if _, isTuple := exprType.(*types.Tuple); isTuple {
			return fmt.Sprintf("(go$obj = %s, %s ? [%s, true] : [%s, false])", c.translateExpr(e.X), check, value, c.zeroValue(c.info.Types[e.Type]))
		}
		return fmt.Sprintf(`(go$obj = %s, %s ? %s : go$typeAssertionFailed(go$obj, %s))`, c.translateExpr(e.X), check, value, c.typeName(t))

	case *ast.Ident:
		if e.Name == "_" {
			panic("Tried to translate underscore identifier.")
		}
		switch o := c.info.Objects[e].(type) {
		case *types.PkgName:
			return c.pkgVars[o.Pkg().Path()]
		case *types.Var, *types.Const:
			return c.objectName(o)
		case *types.Func:
			return c.objectName(o)
		case *types.TypeName:
			return c.typeName(o.Type())
		case *types.Nil:
			return c.zeroValue(c.info.Types[e])
		case nil:
			return e.Name
		default:
			panic(fmt.Sprintf("Unhandled object: %T\n", o))
		}

	case nil:
		return ""

	default:
		panic(fmt.Sprintf("Unhandled expression: %T\n", e))

	}
}

func (c *PkgContext) translateExprSlice(exprs []ast.Expr, desiredType types.Type) []string {
	parts := make([]string, len(exprs))
	for i, expr := range exprs {
		parts[i] = c.translateExprToType(expr, desiredType)
	}
	return parts
}

func (c *PkgContext) translateExprToType(expr ast.Expr, desiredType types.Type) string {
	if desiredType == nil {
		return c.translateExpr(expr)
	}
	if expr == nil {
		return c.zeroValue(desiredType)
	}

	exprType := c.info.Types[expr]

	// TODO should be fixed in go/types
	if _, isSlice := exprType.Underlying().(*types.Slice); isSlice {
		constValue := c.info.Values[expr]
		if constValue != nil && constValue.Kind() == exact.String {
			exprType = types.Typ[types.String]
			c.info.Types[expr] = exprType
		}
	}

	basicExprType, isBasicExpr := exprType.Underlying().(*types.Basic)
	if isBasicExpr && basicExprType.Kind() == types.UntypedNil {
		return c.zeroValue(desiredType)
	}

	switch t := desiredType.Underlying().(type) {
	case *types.Basic:
		switch {
		case t.Info()&types.IsInteger != 0:
			switch {
			case is64Bit(t):
				switch {
				case !is64Bit(basicExprType):
					return fmt.Sprintf("new %s(0, %s)", c.typeName(desiredType), c.translateExpr(expr))
				case !types.IsIdentical(exprType, desiredType):
					return fmt.Sprintf("(go$obj = %s, new %s(go$obj.high, go$obj.low))", c.translateExpr(expr), c.typeName(desiredType))
				}
			case is64Bit(basicExprType):
				if t.Info()&types.IsUnsigned == 0 && basicExprType.Info()&types.IsUnsigned == 0 {
					return fmt.Sprintf("(go$obj = %s, go$obj.low + ((go$obj.high >> 31) * 4294967296))", c.translateExpr(expr))
				}
				return fmt.Sprintf("%s.low", c.translateExpr(expr))
			case basicExprType.Info()&types.IsFloat != 0:
				return fmt.Sprintf("(%s >> 0)", c.translateExpr(expr))
			default:
				return c.translateExpr(expr)
			}
		case t.Info()&types.IsFloat != 0:
			return c.flatten64(expr)
		case t.Info()&types.IsString != 0:
			value := c.translateExpr(expr)
			switch et := exprType.Underlying().(type) {
			case *types.Basic:
				if is64Bit(et) {
					value = fmt.Sprintf("%s.low", value)
				}
				if et.Info()&types.IsNumeric != 0 {
					return fmt.Sprintf("go$encodeRune(%s)", value)
				}
				return value
			case *types.Slice:
				if types.IsIdentical(et.Elem().Underlying(), types.Typ[types.Rune]) {
					return fmt.Sprintf("go$runesToString(%s)", value)
				}
				return fmt.Sprintf("go$bytesToString(%s)", value)
			default:
				panic(fmt.Sprintf("Unhandled conversion: %v\n", et))
			}
		case t.Kind() == types.UnsafePointer:
			if unary, isUnary := expr.(*ast.UnaryExpr); isUnary && unary.Op == token.AND {
				if indexExpr, isIndexExpr := unary.X.(*ast.IndexExpr); isIndexExpr {
					return fmt.Sprintf("go$sliceToArray(%s)", c.translateExprToType(indexExpr.X, types.NewSlice(types.Typ[types.Uint8])))
				}
				if ident, isIdent := unary.X.(*ast.Ident); isIdent && ident.Name == "_zero" {
					return "new Uint8Array(0)"
				}
			}
			if ptr, isPtr := c.info.Types[expr].(*types.Pointer); isPtr {
				if s, isStruct := ptr.Elem().Underlying().(*types.Struct); isStruct {
					array := c.newVariable("_array")
					target := c.newVariable("_struct")
					c.Printf("%s = new Uint8Array(%d);", array, sizes32.Sizeof(s))
					c.Delayed(func() {
						c.Printf("%s = %s;", target, c.translateExpr(expr))
						c.loadStruct(array, target, s)
					})
					return array
				}
			}
		}

	case *types.Slice:
		switch et := exprType.Underlying().(type) {
		case *types.Basic:
			if et.Info()&types.IsString != 0 {
				if types.IsIdentical(t.Elem().Underlying(), types.Typ[types.Rune]) {
					return fmt.Sprintf("new %s(go$stringToRunes(%s))", c.typeName(desiredType), c.translateExpr(expr))
				}
				return fmt.Sprintf("new %s(go$stringToBytes(%s))", c.typeName(desiredType), c.translateExpr(expr))
			}
		case *types.Array, *types.Pointer:
			return fmt.Sprintf("new %s(%s)", c.typeName(desiredType), c.translateExpr(expr))
		}
		_, desiredIsNamed := desiredType.(*types.Named)
		if desiredIsNamed && !types.IsIdentical(exprType, desiredType) {
			return fmt.Sprintf("(go$obj = %s, go$subslice(new %s(go$obj.array), go$obj.offset, go$obj.offset + go$obj.length))", c.translateExpr(expr), c.typeName(desiredType))
		}
		return c.translateExpr(expr)

	case *types.Interface:
		if isWrapped(exprType) {
			return fmt.Sprintf("new %s(%s)", c.typeName(exprType), c.translateExpr(expr))
		}
		if _, isStruct := exprType.Underlying().(*types.Struct); isStruct {
			return fmt.Sprintf("(go$obj = %s, new go$obj.constructor.Struct(go$obj))", c.translateExpr(expr))
		}

	case *types.Pointer:
		n, isNamed := t.Elem().(*types.Named)
		s, isStruct := t.Elem().Underlying().(*types.Struct)

		if isStruct && types.IsIdentical(exprType, types.Typ[types.UnsafePointer]) {
			array := c.newVariable("_array")
			target := c.newVariable("_struct")
			c.Printf("%s = %s;", array, c.translateExpr(expr))
			c.Printf("%s = %s;", target, c.zeroValue(t.Elem()))
			c.loadStruct(array, target, s)
			return target
		}

		if isNamed && !types.IsIdentical(exprType, desiredType) {
			if isStruct {
				return c.clone(c.translateExpr(expr), t.Elem())
			}
			return fmt.Sprintf("(go$obj = %s, new (go$ptrType(%s))(go$obj.go$get, go$obj.go$set))", c.translateExpr(expr), c.typeName(n))
		}

	case *types.Struct, *types.Array:
		if _, isComposite := expr.(*ast.CompositeLit); !isComposite {
			return c.clone(c.translateExpr(expr), desiredType)
		}

	case *types.Chan, *types.Map, *types.Signature:
		// no converion

	default:
		panic(fmt.Sprintf("Unhandled conversion: %v\n", t))
	}

	return c.translateExpr(expr)
}

func (c *PkgContext) clone(src string, ty types.Type) string {
	switch t := ty.Underlying().(type) {
	case *types.Struct:
		structVar := c.newVariable("_struct")
		fields := make([]string, t.NumFields())
		for i := range fields {
			fields[i] = c.clone(structVar+"."+fieldName(t, i), t.Field(i).Type())
		}
		constructor := structVar + ".constructor"
		if named, isNamed := ty.(*types.Named); isNamed {
			constructor = c.objectName(named.Obj()) + ".Ptr"
		}
		return fmt.Sprintf("(%s = %s, new %s(%s))", structVar, src, constructor, strings.Join(fields, ", "))
	case *types.Array:
		return fmt.Sprintf("go$mapArray(%s, function(entry) { return %s; })", src, c.clone("entry", t.Elem()))
	default:
		return src
	}
}

func (c *PkgContext) loadStruct(array, target string, s *types.Struct) {
	view := c.newVariable("_view")
	c.Printf("%s = new DataView(%s.buffer, %s.byteOffset);", view, array, array)
	var fields []*types.Var
	var collectFields func(s *types.Struct, path string)
	collectFields = func(s *types.Struct, path string) {
		for i := 0; i < s.NumFields(); i++ {
			field := s.Field(i)
			if fs, isStruct := field.Type().Underlying().(*types.Struct); isStruct {
				collectFields(fs, path+"."+fieldName(s, i))
				continue
			}
			fields = append(fields, types.NewVar(0, nil, path+"."+fieldName(s, i), field.Type()))
		}
	}
	collectFields(s, target)
	offsets := sizes32.Offsetsof(fields)
	for i, field := range fields {
		switch t := field.Type().Underlying().(type) {
		case *types.Basic:
			if t.Info()&types.IsNumeric != 0 {
				if is64Bit(t) {
					c.Printf("%s = new %s(%s.getUint32(%d, true), %s.getUint32(%d, true));", field.Name(), c.typeName(field.Type()), view, offsets[i]+4, view, offsets[i])
					continue
				}
				c.Printf("%s = %s.get%s(%d, true);", field.Name(), view, toJavaScriptType(t), offsets[i])
			}
			continue
		case *types.Array:
			c.Printf(`%s = new (go$nativeArray("%s"))(%s.buffer, go$min(%s.byteOffset + %d, %s.buffer.byteLength));`, field.Name(), typeKind(t.Elem()), array, array, offsets[i], array)
			continue
		}
		c.Printf("// skipped: %s %s", field.Name(), field.Type().String())
	}
}

func (c *PkgContext) typeCheck(of string, to types.Type) string {
	if in, isInterface := to.Underlying().(*types.Interface); isInterface {
		if in.MethodSet().Len() == 0 {
			return "true"
		}
		return fmt.Sprintf("%s.implementedBy.indexOf(%s) !== -1", c.typeName(to), of)
	}
	return of + " === " + c.typeName(to)
}

func (c *PkgContext) flatten64(expr ast.Expr) string {
	if is64Bit(c.info.Types[expr].Underlying().(*types.Basic)) {
		return fmt.Sprintf("go$flatten64(%s)", c.translateExpr(expr))
	}
	return c.translateExpr(expr)
}

func translateSelection(sel *types.Selection) string {
	var selectors []string
	t := sel.Recv()
	for _, index := range sel.Index() {
		if ptr, isPtr := t.(*types.Pointer); isPtr {
			t = ptr.Elem()
		}
		s := t.Underlying().(*types.Struct)
		selectors = append(selectors, fieldName(s, index))
		t = s.Field(index).Type()
	}
	return strings.Join(selectors, ".")
}

func fixNumber(value string, basic *types.Basic) string {
	switch basic.Kind() {
	case types.Int8:
		return "(" + value + " << 24 >> 24)"
	case types.Uint8:
		return "(" + value + " << 24 >>> 24)"
	case types.Int16:
		return "(" + value + " << 16 >> 16)"
	case types.Uint16:
		return "(" + value + " << 16 >>> 16)"
	case types.Int32:
		return "(" + value + " >> 0)"
	case types.Uint32, types.Uintptr:
		return "(" + value + " >>> 0)"
	}
	return "(" + value + ")"
}

type HasDeferVisitor struct {
	hasDefer bool
}

func (v *HasDeferVisitor) Visit(node ast.Node) (w ast.Visitor) {
	if v.hasDefer {
		return nil
	}
	switch node.(type) {
	case *ast.DeferStmt:
		v.hasDefer = true
		return nil
	case *ast.FuncLit:
		return nil
	}
	return v
}
