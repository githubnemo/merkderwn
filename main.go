package main

import (
	"bytes"

	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"unicode"

	"runtime/pprof"
)

type Converter struct {
	inputLength int

	cursor int

	inInlineMath bool

	in  []rune
	out *bytes.Buffer
}

/* Methods that operate on the input */

// Checks if the cursor has reached the end of the input
func (c *Converter) atEof() bool {
	return c.cursor >= c.inputLength
}

// Returns the character at the given cursor
func (c *Converter) at(cursor int) rune {
	return c.in[cursor]
}

// Returns the character at the cursor
func (c *Converter) current() rune {
	return c.in[c.cursor]
}

// Returns the next character after the cursor
func (c *Converter) next() rune {
	return c.in[c.cursor+1]
}

// Returns the next character after the cursor
func (c *Converter) prev() rune {
	return c.in[c.cursor-1]
}

// Returns the next |n| characters after the cursor (i.e. excluding "current()")
func (c *Converter) lookahead(n int) []rune {
	return c.in[c.cursor+1 : c.cursor+1+n]
}

// Same as "lookahead" with a given cursor
func (c *Converter) lookaheadAt(n int, cursor int) []rune {
	return c.in[cursor+1 : cursor+1+n]
}

// Returns the previous |n| characters before the cursor (i.e. excluding "current()")
func (c *Converter) lookback(n int) []rune {
	return c.in[c.cursor-n : c.cursor]
}

/* Methods that operate on the output */

// Writes a string to the output buffer
func (c *Converter) emit(s []rune) {
	c.out.WriteString(string(s))
}

func (c *Converter) emitRune(s rune) {
	c.out.WriteRune(s)
}

/* Parsing \o/ */

// Everything inside an HTML comment is considered to be Latex and thus emitted 1:1
func (c *Converter) handleComments() bool {
	if c.current() != '<' || !eq(c.lookahead(3), []rune("!--")) {
		return false
	}

	for !c.atEof() && (c.current() != '-' || !eq(c.lookahead(2), []rune("->"))) {
		c.emitRune(c.current())
		c.cursor += 1
	}
	c.emit([]rune("-->"))
	c.cursor += 3

	return true
}

// CDATA blocks are comments which are completely dropped from the output
func (c *Converter) handleCDATA() bool {
	if c.current() != '<' || !eq(c.lookahead(8), []rune("![CDATA[")) {
		return false
	}

	for !c.atEof() && (c.current() != ']' || !eq(c.lookahead(2), []rune("]>"))) {
		c.cursor += 1
	}
	c.cursor += 3 // For ]]>

	return true
}

func (c *Converter) handleLatex() bool {
	if !c.inInlineMath && c.current() == '\\' && c.next() != '\\' {
		if eq(c.lookahead(5), []rune("begin")) {
			c.handleLatexBlock()
		} else {
			c.handleLatexCommand(true)
		}
		return true
	}
	return false
}

func (c *Converter) handleLatexCommand(emitCommentBlock bool) {
	if emitCommentBlock {
		c.emit([]rune("<!--"))
	}

	// The command name
	for !c.atEof() &&
		c.current() != '{' &&
		c.current() != '[' &&
		!unicode.IsSpace(c.current()) {

		c.emitRune(c.current())
		c.cursor += 1
	}

	nesting := 0
	for !c.atEof() {
		// All parameters are closed and there is no next parameter,
		// i.e. \foo{bar}{baz} test 123
		//                    ^
		if nesting == 0 && c.current() != '{' && c.current() != '[' {
			break
		}

		// This will break if there's an unbalanced number of different
		// brace types, i.e. "[[]}" will result in nesting = 0. Don't care
		// to fix that right now.
		if c.current() == '{' || c.current() == '[' {
			nesting += 1
		}

		if c.current() == '}' || c.current() == ']' {
			nesting -= 1
		}

		c.emitRune(c.current())
		c.cursor += 1
	}

	if emitCommentBlock {
		c.emit([]rune("-->"))
	}
}

func eq(a,b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

// Handles (nested) \begin{} ... \end{} blocks. Does not care wether you're
// starting/ending the right environment, i.e. this will work:
//
//      \begin{figure} ... \end{math}
//
func (c *Converter) handleLatexBlock() {
	c.emit([]rune("<!--"))
	nesting := 0

	for !c.atEof() {
		if c.current() == '\\' && eq(c.lookahead(5), []rune("begin")) {
			nesting += 1
		} else if c.current() == '\\' && eq(c.lookahead(3), []rune("end")) {
			nesting -= 1
		}

		// If we're at the last \end, we can just parse it as a command, e.g.:
		//
		//      \end{figure*}
		//      ^
		//
		// At that point, handleLatexCommand will consume everything including
		// "}" and then return.
		if nesting == 0 {
			c.handleLatexCommand(false)
			c.emit([]rune("-->"))
			break
		}

		c.emitRune(c.current())
		c.cursor += 1
	}
}

func (c *Converter) handleInlineMath() bool {
	if c.current() == '\\' && c.next() == '$' {
		c.emit([]rune("\\$"))
		c.cursor += 2
		return true
	}

	if c.current() != '$' {
		return false
	}

	// From http://fletcher.github.io/MultiMarkdown-4/math.html:
	// In order to be correctly parsed as math, there must not be any space
	// between the $ and the actual math on the inside of the delimiter, and
	// there must be space on the outside.
	if c.cursor > 0 && c.in[c.cursor-1] == ' ' && !c.inInlineMath {
		c.inInlineMath = true
	} else if c.lookahead(1)[0] == ' ' && c.inInlineMath {
		c.inInlineMath = false
	}

	return false
}

// Conversion loop iterating over all characters. Not very efficient, but does its job.
func (c *Converter) Convert() []byte {
	for !c.atEof() {
		if c.handleComments() {
			continue
		}

		if c.handleCDATA() {
			continue
		}

		if c.handleInlineMath() {
			continue
		}

		if c.handleLatex() {
			continue
		}

		c.emitRune(c.current())
		c.cursor += 1
	}

	return c.out.Bytes()
}

/* Utility */

func ByteArrayToConverter(in []byte) Converter {
	c := Converter{
		cursor:      0,
		in:          bytes.Runes(in),
		out:         new(bytes.Buffer),
	}

	c.inputLength = len(c.in)

	return c
}

func SXMD(in []byte) []byte {
	c := ByteArrayToConverter(in)
	return c.Convert()
}

var flag_cpuProfile = flag.String("cpuprofile", "", "lol")

func main() {
	flag.Parse()
	if len(flag.Args()) != 1 {
		fmt.Printf("Usage: %s <file-to-convert>\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	}

	if *flag_cpuProfile != "" {
		f, err := os.Create(*flag_cpuProfile)
        if err != nil {
            panic(err)
        }
        pprof.StartCPUProfile(f)
        defer pprof.StopCPUProfile()
	}

	inputFilePath := flag.Arg(0)
	content, err := ioutil.ReadFile(inputFilePath)
	if err != nil {
		fmt.Printf("Could not read input file %s", inputFilePath)
		os.Exit(1)
	}

	content = SXMD(content)
	os.Stdout.Write(content)
}
