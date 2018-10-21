package participle

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/alecthomas/participle/lexer"
)

var (
	positionType  = reflect.TypeOf(lexer.Position{})
	captureType   = reflect.TypeOf((*Capture)(nil)).Elem()
	parseableType = reflect.TypeOf((*Parseable)(nil)).Elem()

	// NextMatch should be returned by Parseable.Parse() method implementations to indicate
	// that the node did not match and that other matches should be attempted, if appropriate.
	NextMatch = errors.New("no match") // nolint: golint
)

// Context for a single parse.
type parseContext struct {
	lexer.PeekingLexer
	caseInsensitive map[rune]bool
}

// A node in the grammar.
type node interface {
	// Parse from scanner into value.
	//
	// Returned slice will be nil if the node does not match.
	Parse(ctx parseContext, parent reflect.Value) ([]reflect.Value, error)

	// Return a decent string representation of the Node.
	String() string
}

func decorate(err *error, name func() string) {
	if *err == nil {
		return
	}
	switch realError := (*err).(type) {
	case *lexer.Error:
		*err = &lexer.Error{Message: name() + ": " + realError.Message, Pos: realError.Pos}
	default:
		*err = fmt.Errorf("%s: %s", name(), realError)
	}
}

// A node that proxies to an implementation that implements the Parseable interface.
type parseable struct {
	t reflect.Type
}

func (p *parseable) String() string { return stringer(p) }

func (p *parseable) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	rv := reflect.New(p.t)
	v := rv.Interface().(Parseable)
	err = v.Parse(ctx)
	if err != nil {
		if err == NextMatch {
			return nil, nil
		}
		return nil, err
	}
	return []reflect.Value{rv.Elem()}, nil
}

type strct struct {
	typ  reflect.Type
	expr node
}

func (s *strct) String() string { return stringer(s) }

func (s *strct) maybeInjectPos(pos lexer.Position, v reflect.Value) {
	if f := v.FieldByName("Pos"); f.IsValid() && f.Type() == positionType {
		f.Set(reflect.ValueOf(pos))
	}
}

func (s *strct) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	sv := reflect.New(s.typ).Elem()
	t, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	s.maybeInjectPos(t.Pos, sv)
	if out, err = s.expr.Parse(ctx, sv); err != nil {
		return nil, err
	} else if out == nil {
		return nil, nil
	}
	return []reflect.Value{sv}, nil
}

// <expr> {"|" <expr>}
type disjunction struct {
	nodes     []node
	lookahead lookaheadTable
}

func (d *disjunction) String() string { return stringer(d) }

func (d *disjunction) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	if selected, err := d.lookahead.Select(ctx, parent); err != nil {
		return nil, err
	} else if selected != -2 {
		if selected == -1 {
			return nil, nil
		}
		return d.nodes[selected].Parse(ctx, parent)
	}

	// Same logic without lookahead.
	for _, a := range d.nodes {
		if value, err := a.Parse(ctx, parent); err != nil {
			return nil, err
		} else if value != nil {
			return value, nil
		}
	}
	return nil, nil
}

// <node> ...
type sequence struct {
	head bool
	node node
	next *sequence
}

func (s *sequence) String() string { return stringer(s) }

func (s *sequence) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	for n := s; n != nil; n = n.next {
		child, err := n.node.Parse(ctx, parent)
		if err != nil {
			return nil, err
		}
		if child == nil {
			// Early exit if first value doesn't match, otherwise all values must match.
			if n == s {
				return nil, nil
			}
			token, err := ctx.Peek(0)
			if err != nil {
				return nil, err
			}
			return nil, lexer.Errorf(token.Pos, "unexpected %q (expected %s)", token, n)
		}
		out = append(out, child...)
	}
	return out, nil
}

// @<expr>
type capture struct {
	field structLexerField
	node  node
}

func (c *capture) String() string { return stringer(c) }

func (c *capture) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	token, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	pos := token.Pos
	v, err := c.node.Parse(ctx, parent)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return []reflect.Value{parent}, setField(pos, parent, c.field, v)
}

// <identifier> - named lexer token reference
type reference struct {
	typ        rune
	identifier string // Used for informational purposes.
}

func (r *reference) String() string { return stringer(r) }

func (r *reference) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	token, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	if token.Type != r.typ {
		return nil, nil
	}
	_, _ = ctx.Next()
	return []reflect.Value{reflect.ValueOf(token.Value)}, nil
}

// [ <expr> ] <sequence>
type optional struct {
	node      node
	next      node
	lookahead lookaheadTable
}

func (o *optional) String() string { return stringer(o) }

func (o *optional) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	result, err := o.lookahead.Select(ctx, parent)
	if err != nil {
		return nil, err
	}
	switch result {
	case -2: // No lookahead table
		fallthrough
	case 0:
		out, err = o.node.Parse(ctx, parent)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = []reflect.Value{}
		}
		fallthrough
	case 1:
		if o.next != nil {
			next, err := o.next.Parse(ctx, parent)
			if err != nil {
				return nil, err
			}
			if next == nil {
				return nil, nil
			}
			out = append(out, next...)
		}
		return out, nil
	case -1:
		// We have a next node but neither it or the optional matched the lookahead, so it's a complete mismatch.
		if o.next != nil {
			return nil, nil
		}
		return []reflect.Value{}, nil
	default:
		panic("unexpected selection")
	}
}

// { <expr> } <sequence>
type repetition struct {
	node      node
	next      node
	lookahead lookaheadTable
}

func (r *repetition) String() string { return stringer(r) }

// Parse a repetition. Once a repetition is encountered it will always match, so grammars
// should ensure that branches are differentiated prior to the repetition.
func (r *repetition) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	result, err := r.lookahead.Select(ctx, parent)
	if err != nil {
		return nil, err
	}
	switch result {
	case -2: // No lookahead table
		fallthrough
	case 0:
		for {
			v, err := r.node.Parse(ctx, parent)
			if err != nil {
				return nil, err
			}
			if v == nil {
				break
			}
			out = append(out, v...)
		}
		if out == nil {
			out = []reflect.Value{}
		}
		fallthrough
	case 1:
		if r.next != nil {
			next, err := r.next.Parse(ctx, parent)
			if err != nil {
				return nil, err
			}
			if next == nil {
				return nil, nil
			}
			out = append(out, next...)
		}
		return out, nil
	case -1:
		// We have a next node but neither it or the optional matched the lookahead, so it's a complete mismatch.
		if r.next != nil {
			return nil, nil
		}
		return []reflect.Value{}, nil
	default:
		panic("unexpected selection")
	}
}

// Match a token literal exactly "..."[:<type>].
type literal struct {
	s  string
	t  rune
	tt string // Used for display purposes - symbolic name of t.
}

func (l *literal) String() string { return stringer(l) }

func (l *literal) Parse(ctx parseContext, parent reflect.Value) (out []reflect.Value, err error) {
	token, err := ctx.Peek(0)
	if err != nil {
		return nil, err
	}
	equal := false // nolint: ineffassign
	if ctx.caseInsensitive[token.Type] {
		equal = strings.EqualFold(token.Value, l.s)
	} else {
		equal = token.Value == l.s
	}
	if equal && (l.t == -1 || l.t == token.Type) {
		next, err := ctx.Next()
		if err != nil {
			return nil, err
		}
		return []reflect.Value{reflect.ValueOf(next.Value)}, nil
	}
	return nil, nil
}

// Attempt to transform values to given type.
//
// This will dereference pointers, and attempt to parse strings into integer values, floats, etc.
func conform(t reflect.Type, values []reflect.Value) (out []reflect.Value, err error) {
	for _, v := range values {
		for t != v.Type() && t.Kind() == reflect.Ptr && v.Kind() != reflect.Ptr {
			v = v.Addr()
		}

		// Already of the right kind, don't bother converting.
		if v.Kind() == t.Kind() {
			out = append(out, v)
			continue
		}

		kind := t.Kind()
		switch kind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n, err := strconv.ParseInt(v.String(), 0, sizeOfKind(kind))
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q: %s", v.String(), err)
			}
			v = reflect.New(t).Elem()
			v.SetInt(n)

		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n, err := strconv.ParseUint(v.String(), 0, sizeOfKind(kind))
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q: %s", v.String(), err)
			}
			v = reflect.New(t).Elem()
			v.SetUint(n)

		case reflect.Bool:
			v = reflect.ValueOf(true)

		case reflect.Float32, reflect.Float64:
			n, err := strconv.ParseFloat(v.String(), sizeOfKind(kind))
			if err != nil {
				return nil, fmt.Errorf("invalid integer %q: %s", v.String(), err)
			}
			v = reflect.New(t).Elem()
			v.SetFloat(n)
		}

		out = append(out, v)
	}
	return out, nil
}

func sizeOfKind(kind reflect.Kind) int {
	switch kind {
	case reflect.Int8, reflect.Uint8:
		return 8
	case reflect.Int16, reflect.Uint16:
		return 16
	case reflect.Int32, reflect.Uint32, reflect.Float32:
		return 32
	case reflect.Int64, reflect.Uint64, reflect.Float64:
		return 64
	case reflect.Int, reflect.Uint:
		return strconv.IntSize
	}
	panic("unsupported kind " + kind.String())
}

// Set field.
//
// If field is a pointer the pointer will be set to the value. If field is a string, value will be
// appended. If field is a slice, value will be appended to slice.
//
// For all other types, an attempt will be made to convert the string to the corresponding
// type (int, float32, etc.).
func setField(pos lexer.Position, strct reflect.Value, field structLexerField, fieldValue []reflect.Value) (err error) { // nolint: gocyclo
	defer decorate(&err, func() string { return pos.String() + ": " + strct.Type().String() + "." + field.Name })

	f := strct.FieldByIndex(field.Index)
	switch f.Kind() {
	case reflect.Slice:
		fieldValue, err = conform(f.Type().Elem(), fieldValue)
		if err != nil {
			return err
		}
		f.Set(reflect.Append(f, fieldValue...))
		return nil

	case reflect.Ptr:
		if f.IsNil() {
			fv := reflect.New(f.Type().Elem()).Elem()
			f.Set(fv.Addr())
			f = fv
		} else {
			f = f.Elem()
		}
	}

	if f.Kind() == reflect.Struct {
		if pf := f.FieldByName("Pos"); pf.IsValid() && pf.Type() == positionType {
			pf.Set(reflect.ValueOf(pos))
		}
	}

	if f.CanAddr() {
		if d, ok := f.Addr().Interface().(Capture); ok {
			ifv := []string{}
			for _, v := range fieldValue {
				ifv = append(ifv, v.Interface().(string))
			}
			err := d.Capture(ifv)
			if err != nil {
				return err
			}
			return nil
		}
	}

	// Strings concatenate all captured tokens.
	if f.Kind() == reflect.String {
		fieldValue, err = conform(f.Type(), fieldValue)
		if err != nil {
			return err
		}
		for _, v := range fieldValue {
			f.Set(reflect.ValueOf(f.String() + v.String()).Convert(f.Type()))
		}
		return nil
	}

	// Coalesce multiple tokens into one. This allows eg. ["-", "10"] to be captured as separate tokens but
	// parsed as a single string "-10".
	if len(fieldValue) > 1 {
		out := []string{}
		for _, v := range fieldValue {
			out = append(out, v.String())
		}
		fieldValue = []reflect.Value{reflect.ValueOf(strings.Join(out, ""))}
	}

	fieldValue, err = conform(f.Type(), fieldValue)
	if err != nil {
		return err
	}

	fv := fieldValue[0]

	switch f.Kind() {
	// Numeric types will increment if the token can not be coerced.
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if fv.Type() != f.Type() {
			f.SetInt(f.Int() + 1)
		} else {
			f.Set(fv)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if fv.Type() != f.Type() {
			f.SetUint(f.Uint() + 1)
		} else {
			f.Set(fv)
		}

	case reflect.Float32, reflect.Float64:
		if fv.Type() != f.Type() {
			f.SetFloat(f.Float() + 1)
		} else {
			f.Set(fv)
		}

	case reflect.Bool, reflect.Struct:
		if fv.Type() != f.Type() {
			return fmt.Errorf("value %q is not correct type %s", fv, f.Type())
		}
		f.Set(fv)

	default:
		return fmt.Errorf("unsupported field type %s for field %s", f.Type(), field.Name)
	}
	return nil
}

func indirectType(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		return indirectType(t.Elem())
	}
	return t
}

func panicf(f string, args ...interface{}) {
	panic(Error(fmt.Sprintf(f, args...)))
}

// Error is an error returned by the parser internally to differentiate from non-Participle errors.
type Error string

func (e Error) Error() string { return string(e) }
