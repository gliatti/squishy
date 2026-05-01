package mysql

import "fmt"

type ParseError struct {
	Pos      Position
	Msg      string
	Got      string
	Expected string
}

func (e *ParseError) Error() string {
	if e.Expected != "" {
		return fmt.Sprintf("parse error at %d:%d: %s (got %q, expected %s)",
			e.Pos.Line, e.Pos.Col, e.Msg, e.Got, e.Expected)
	}
	return fmt.Sprintf("parse error at %d:%d: %s (got %q)",
		e.Pos.Line, e.Pos.Col, e.Msg, e.Got)
}

type ErrorList []*ParseError

func (l ErrorList) Error() string {
	if len(l) == 0 {
		return "no errors"
	}
	if len(l) == 1 {
		return l[0].Error()
	}
	return fmt.Sprintf("%s (and %d more)", l[0].Error(), len(l)-1)
}
