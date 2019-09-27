package stcdetail

import "bytes"
import "container/list"
import "fmt"
import "io"
import "io/ioutil"
import "os"
import "reflect"
import "strings"

const tabwidth = 8
const eofRune rune = -1

var ErrInvalidNumArgs = fmt.Errorf("invalid number of arguments")
var ErrInvalidSection = fmt.Errorf("syntactically invalid section")

// Test if a string is a valid INI file section name.  Section names
// cannot be the empty string and must consist only of alphanumeric
// characters and '-'.
func ValidIniSection(s string) bool {
	return len(s) > 0 && -1 == strings.IndexFunc(s, func(r rune)bool {
		return !isKeyChar(r)
	})
}

// Test if a string is a valid subsection name in an INI file.
// Specifically, subsection names may not contain a newline or NUL
// byte.
func ValidIniSubsection(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\000' {
			return false
		}
	}
	return true
}

// Section of an INI file.  A nil *IniSection corresponds to the
// "section-free" part of the file before the first section, which the
// git-config man page says is not valid, but the git-config tool
// halfway supports.
type IniSection struct {
	Section    string
	Subsection *string
}

// Returns false if either the section or subsection is illegal.
// Returns true for a nil *IniSection.
func (s *IniSection) Valid() bool {
	if s == nil {
		return true
	} else if !ValidIniSection(s.Section) {
		return false
	}
	return s.Subsection == nil || ValidIniSubsection(*s.Subsection)
}

// Renders as [section] or [section "subsection"].  The nil
// *IniSection renders as an empty string.  Panics if the subsection
// includes the illegal characters '\n' or '\000'.
func (s *IniSection) String() string {
	if s == nil {
		return ""
	} else if s.Subsection != nil {
		ret := strings.Builder{}
		fmt.Fprintf(&ret, "[%s \"", s.Section)
		for i := 0; i < len(*s.Subsection); i++ {
			switch b := (*s.Subsection)[i]; b {
			case '\n', '\000':
				panic("illegal character in IniSection Subsection")
			case '\\', '"':
				ret.WriteByte('\\')
				fallthrough
			default:
				ret.WriteByte(b)
			}
		}
		ret.WriteString("\"]")
		return ret.String()
	}
	return fmt.Sprintf("[%s]", s.Section)
}

// True if two *IniSection have the same contents.
func (s *IniSection) Eq(s2 *IniSection) bool {
	if s == nil && s2 == nil {
		return true
	} else if s == nil || s2 == nil {
		return false
	} else if s.Section != s2.Section {
		return false
	} else if s.Subsection == nil && s2.Subsection == nil {
		return true
	} else if s.Subsection == nil || s2.Subsection == nil {
		return false
	}
	return *s.Subsection == *s2.Subsection
}

// Produce a fully "qualified" key consisting of the section, optional
// subsection, and key separated by dots, as understood by the
// git-config command.
func IniQKey(s *IniSection, key string) string {
	if !s.Valid() {
		panic(fmt.Sprintf("illegal INI section %s", s.String()))
	} else if !ValidIniKey(key) {
		panic(fmt.Sprintf("illegal INI key %q", key))
	} else if s == nil {
		return key
	} else if s.Subsection == nil {
		return s.Section + "." + key
	}
	return s.Section + "." + *s.Subsection + "." + key
}

type IniRange struct {
	// The text of a key, value pair or section header lies between
	// StartIndex and EndIndex.  If PrevEndIndex != StartIndex, then
	// the bytes between PrevEndIndex and StartIndex constitute a
	// comment or blank lines.
	StartIndex, EndIndex, PrevEndIndex int

	// The entire input file
	Input []byte
}

type IniItem struct {
	*IniSection
	Key string
	Value *string
	IniRange
}

// Returns Value or an empty string if Value is nil.
func (ii *IniItem) Val() string {
	if ii.Value == nil {
		return ""
	}
	return *ii.Value
}

// Returns the Key qualified by the section (see IniQKey).
func (ii *IniItem) QKey() string {
	return IniQKey(ii.IniSection, ii.Key)
}

// Type that receives and processes the parsed INI file.  Note that if
// there is also Section(IniSecStart)error method, this is called at
// the start of sections, and if there is a Done(IniRange) method it
// is called at the end of the file.
type IniSink interface {
	// optional:
	// Section(IniSecStart) error
	// Init()
	// Done()
	//
	Item(IniItem) error
}

type IniSecStart struct {
	IniSection
	IniRange
}

// Error that an IniSink's Value method should return when there is a
// problem with the key, rather than a problem with the value.  By
// default, the line and column number of an error will correspond to
// the start of the value, but with BadKey the error will point to the
// key.
type BadKey string

func (err BadKey) Error() string {
	return string(err)
}

// Just a random error type useful for bad values in INI files.
// Exists for symmetry with BadKey, though BadValue is in no way
// special.
type BadValue string

func (err BadValue) Error() string {
	return string(err)
}

// A single parse error in an IniFile.
type ParseError struct {
	File          string
	Lineno, Colno int
	Msg           string
}

func (err ParseError) Error() string {
	if err.File == "" {
		return fmt.Sprintf("%d:%d: %s", err.Lineno, err.Colno, err.Msg)
	}
	return fmt.Sprintf("%s:%d:%d: %s", err.File, err.Lineno, err.Colno, err.Msg)
}

// The collection of parse errors that resulted from parsing a file.
type ParseErrors []ParseError

func (err ParseErrors) Error() string {
	ret := &strings.Builder{}
	for i, e := range err {
		if i != 0 {
			ret.WriteByte('\n')
		}
		ret.WriteString(e.Error())
	}
	return ret.String()
}

type position struct {
	index, lineno, colno int
}

type iniParse struct {
	position
	input   []byte
	file    string
	sec     *IniSection
	prevEnd int
	done    func(IniRange)
	Value   func(IniItem) error
	Section func(sec IniSecStart) error
}

func (l *iniParse) throwAt(pos position, msg string) {
	panic(ParseError{
		File:   l.file,
		Lineno: pos.lineno + 1,
		Colno:  pos.colno + 1,
		Msg:    msg,
	})
}

func (l *iniParse) throw(msg string, args ...interface{}) {
	l.throwAt(l.position, fmt.Sprintf(msg, args...))
}

func (l *iniParse) peek() rune {
	if l.index >= len(l.input) {
		return eofRune
	}
	return rune(l.input[l.index])
}

func (l *iniParse) at(n int) rune {
	n += l.index
	if n > len(l.input) || n < 0 {
		return eofRune
	}
	return rune(l.input[n])
}

func (l *iniParse) remaining() int {
	return len(l.input) - l.index
}

func (l *iniParse) skip(n int) {
	if n < 0 || n > l.remaining() {
		n = l.remaining()
	}
	i := l.index
	stop := i + n
	for ; i < stop; i++ {
		switch l.input[i] {
		case '\n':
			l.lineno++
			l.colno = 0
		case '\t':
			l.colno += 8 - (l.colno % tabwidth)
		default:
			l.colno++
		}
	}
	l.index = i
}

func (l *iniParse) take(n int) string {
	i := l.index
	l.skip(n)
	return string(l.input[i:l.index])
}

func (l *iniParse) match(text string) bool {
	n := len(text)
	if l.remaining() >= n && string(l.input[l.index:l.index+n]) == text {
		l.skip(n)
		return true
	}
	return false
}

func (l *iniParse) skipWhile(fn func(rune) bool) bool {
	i := l.index
	for ; i < len(l.input) && fn(rune(l.input[i])); i++ {
	}
	if i > l.index {
		l.skip(i - l.index)
		return true
	}
	return false
}

func (l *iniParse) skipTo(c byte) bool {
	if i := bytes.IndexByte(l.input[l.index:], c); i >= 0 {
		l.skip(i)
		return true
	}
	l.skip(l.remaining())
	return false
}

func (l *iniParse) takeWhile(fn func(rune) bool) string {
	i := l.index
	l.skipWhile(fn)
	return string(l.input[i:l.index])
}

func (l *iniParse) skipWS() bool {
	return l.skipWhile(func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r'
	})
}

func isAlpha(c rune) bool {
	c &^= 0x20
	return c >= 'A' && c <= 'Z'
}
func isKeyChar(c rune) bool {
	return isAlpha(c) || (c >= '0' && c <= '9') || c == '-'
}

// Test if string is a valid INI file key.  Valid keys start with a
// letter followed by zero or more alphanumeric characters or '-'
// characters.
func ValidIniKey(s string) bool {
	return s != "" && isAlpha(rune(s[0])) &&
		-1 == strings.IndexFunc(s, func(r rune)bool {
		return !isKeyChar(r)
	})
}

func (l *iniParse) getKey() string {
	return l.takeWhile(isKeyChar)
}

func (l *iniParse) getSubsection() *string {
	if l.remaining() < 2 || l.peek() != '"' {
		return nil
	}
	ret := &strings.Builder{}
	var i int
loop:
	for i = l.index + 1; i+1 < len(l.input); i++ {
		switch c := l.input[i]; c {
		case '"':
			break loop
		case '\000', '\n':
			return nil
		case '\\':
			nc := l.input[i+1]
			if nc == '\\' || nc == '"' {
				ret.WriteByte(nc)
			}
			i++
		default:
			ret.WriteByte(c)
		}
	}
	if l.input[i] != '"' {
		return nil
	}
	l.skip(i + 1 - l.index)
	s := ret.String()
	return &s
}

func (l *iniParse) getSection() *IniSection {
	if !l.match("[") {
		return nil
	}
	var ret IniSection
	ret.Section = l.getKey()
	if len(ret.Section) == 0 {
		l.throw("expected section name after '['")
	}
	if l.match("]") {
		return &ret
	}
	if !l.skipWS() {
		l.throw("expected ']' or space followed by quoted subsection")
	}
	if ret.Subsection = l.getSubsection(); ret.Subsection == nil {
		l.throw("expected quoted subsection after space")
	}
	if !l.match("]") {
		l.throw("expected ']'")
	}
	return &ret
}

func needQuotes(val string) bool {
	if val == "" {
		return false
	} else if val[0] == ' ' || val[0] == '\t' {
		return true
	}
	for _, c := range ([]byte)(val) {
		if c < ' ' || c >= 0x7f || strings.IndexByte("\"#;\\", c) != -1 {
			return true
		}
	}
	return false
}

func EscapeIniValue(val string) string {
	if !needQuotes(val) {
		return val
	}
	ret := strings.Builder{}
	ret.WriteByte('"')
	for _, b := range []byte(val) {
		switch b {
		case '"', '\\':
			ret.WriteByte('\\')
			ret.WriteByte(b)
		case '\b':
			ret.WriteString("\\b")
		case '\n':
			ret.WriteString("\\n")
		case '\t':
			ret.WriteString("\\t")
		default:
			ret.WriteByte(b)
		}
	}
	ret.WriteByte('"')
	return ret.String()
}

func (l *iniParse) getValue() string {
	ret := strings.Builder{}
	escape, inquote := false, false
	for {
		c := l.peek()
		if escape {
			escape = false
			switch c {
			case '"', '\\':
				ret.WriteByte(byte(c))
			case 'n':
				ret.WriteByte('\n')
			case 't':
				ret.WriteByte('\t')
			case 'b':
				ret.WriteByte('\b')
			case '\n':
				// ignore
			case '\r':
				if l.at(1) == '\n' {
					l.skip(1)
					break
				}
				fallthrough
			default:
				if c == eofRune {
					l.throw("incomplete escape sequence at EOF")
				}
				l.throw("invalid escape sequence \\%c", c)
			}
		} else if c == '\\' {
			escape = true
		} else if c == '"' {
			inquote = !inquote
		} else if c == '\n' || c == eofRune || (c == '\r' && l.at(1) == '\n') {
			if c == '\r' {
				l.skip(1)
			}
			if inquote {
				l.throw("missing close quotes")
			}
			l.skip(1)
			return ret.String()
		} else if !inquote && (c == '#' || c == ';') {
			l.skipTo('\n')
		} else {
			ret.WriteByte(byte(c))
		}
		l.skip(1)
	}
}

func (l *iniParse) getRange(startIdx int) IniRange {
	prev := l.prevEnd
	l.prevEnd = l.index
	return IniRange{
		StartIndex: startIdx,
		EndIndex: l.index,
		PrevEndIndex: prev,
		Input: l.input,
	}
}

func (l *iniParse) do1() (err *ParseError) {
	defer func() {
		if i := recover(); i != nil {
			if pe, ok := i.(ParseError); ok {
				err = &pe
				l.skipTo('\n')
			} else {
				panic(i)
			}
		}
	}()
	startindex := l.index
	l.skipWS()
	keypos := l.position
	if sec := l.getSection(); sec != nil {
		l.skipWS()
		l.match("\n")
		l.sec = sec
		if err := l.Section(IniSecStart{
			IniSection: *sec,
			IniRange: l.getRange(startindex),
		}); err != nil {
			l.throwAt(keypos, err.Error())
		}
	} else if isAlpha(l.peek()) {
		k := l.getKey()
		l.skipWS()
		var v *string
		var valpos position
		if !l.match("=") {
			if c := l.peek(); c != '\n' && c != '#' &&
				c != ';' && c != eofRune {
				l.throw("Expected '=' after %s", k)
			}
			valpos = l.position
			if l.skipTo('\n') {
				l.skip(1)
			}
		} else {
			l.skipWS()
			valpos = l.position
			val := l.getValue()
			v = &val
		}
		if err := l.Value(IniItem{
			IniSection: l.sec,
			Key:        k,
			Value:      v,
			IniRange:   l.getRange(startindex),
			}); err != nil {
			if ke, ok := err.(BadKey); ok {
				l.throwAt(keypos, string(ke))
			} else {
				l.throwAt(valpos, err.Error())
			}
		}
	} else if c := l.peek(); c == '#' || c == ';' || c == '\n' {
		l.skipTo('\n')
		l.skip(1)
	} else {
		l.throw("Expected section or key")
	}
	return
}

func (l *iniParse) do() error {
	var err ParseErrors
	for l.remaining() > 0 {
		if e := l.do1(); e != nil {
			err = append(err, *e)
		}
	}
	l.done(l.getRange(l.index))
	if err == nil {
		return nil
	}
	return err
}

func newParser(sink IniSink, path string, input []byte) *iniParse {
	var ret iniParse
	ret.file = path
	ret.input = input
	ret.Value = sink.Item
	if iss, ok := sink.(interface{Section(IniSecStart)error}); ok {
		ret.Section = iss.Section
	} else {
		ret.Section = func(IniSecStart) error { return nil }
	}
	if done, ok := sink.(interface{ Done(IniRange) }); ok {
		ret.done = done.Done
	} else {
		ret.done = func(IniRange){}
	}
	if init, ok := sink.(interface{ Init() }); ok {
		init.Init()
	}
	return &ret
}

// Parse the contents of an INI file.  The filename argument is used
// only for error messages.
func IniParseContents(sink IniSink, filename string, contents []byte) error {
	return newParser(sink, filename, contents).do()
}

// Open, read, and parse an INI file.  If the file is incorrectly
// formatted, will return an error of type ParseErrors.
func IniParse(sink IniSink, filename string) error {
	if f, err := os.Open(filename); err != nil {
		return err
	} else {
		contents, err := ioutil.ReadAll(f)
		f.Close()
		if err != nil {
			return err
		}
		return newParser(sink, filename, contents).do()
	}
}

// You can parse an INI file into an IniEditor, Set, Del, or Add
// key-value pairs, then write out the result using WriteTo.
// Preserves most comments and file ordering.
type IniEditor struct {
	fragments list.List
	secEnd    map[string]*list.Element
	values    map[string][]*list.Element
	lastSec   *IniSection
}

// Write the contents of IniEditor to a Writer after applying edits
// have been made.
func (ie *IniEditor) WriteTo(w io.Writer) (int64, error) {
	var ret int64
	for e := ie.fragments.Front(); e != nil; e = e.Next() {
		n, err := w.Write(e.Value.([]byte))
		ret += int64(n)
		if err != nil {
			return ret, err
		}
	}
	return ret, nil
}

func (ie *IniEditor) String() string {
	ret := strings.Builder{}
	ie.WriteTo(&ret)
	return ret.String()
}

// Delete all instances of a key from the file.
func (ie *IniEditor) Del(is *IniSection, key string) {
	k := IniQKey(is, key)
	for _, e := range ie.values[k] {
		ie.fragments.Remove(e)
	}
	delete(ie.values, k)
}

func iniLine(key, value string) []byte {
	return []byte(fmt.Sprintf("\t%s = %s\n", key, EscapeIniValue(value)))
}

func (ie *IniEditor) newItem(is *IniSection, key, value string) *list.Element {
	ss := is.String()
	e, ok := ie.secEnd[ss]
	if !ok {
		e = ie.fragments.Back()
		if ssb := []byte(ss+"\n"); e != nil && len(e.Value.([]byte)) == 0 {
			e.Value = ssb
		} else {
			e = ie.fragments.PushBack(ssb)
		}
		e = ie.fragments.InsertAfter([]byte{}, e)
		ie.secEnd[ss] = e
	}
	e = ie.fragments.InsertBefore(iniLine(key, value), e)
	k := IniQKey(is, key)
	ie.values[k] = append(ie.values[k], e)
	return e
}

// Replace all instances of key with a single one equal to value.
func (ie *IniEditor) Set(is *IniSection, key, value string) {
	k := IniQKey(is, key)
	vs := ie.values[k]
	if len(vs) > 0 {
		ie.values[k] = []*list.Element{
			ie.fragments.InsertAfter(iniLine(key, value), vs[len(vs)-1]),
		}
		for _, e := range vs {
			ie.fragments.Remove(e)
		}
	} else {
		ie.newItem(is, key, value)
	}
}

// Add a new instance of key to the file without deleting any previous
// instance of the key.
func (ie *IniEditor) Add(is *IniSection, key, value string) {
	k := IniQKey(is, key)
	vs := ie.values[k]
	if len(vs) > 0 {
		e := ie.fragments.InsertAfter(iniLine(key, value), vs[len(vs)-1])
		ie.values[k] = append(vs, e)
	} else {
		ie.newItem(is, key, value)
	}
}

func (ie *IniEditor) appendItem(r *IniRange) (e1, e2 *list.Element) {
	if r.StartIndex > r.PrevEndIndex {
		e1 = ie.fragments.PushBack(r.Input[r.PrevEndIndex:r.StartIndex])
	}
	if r.EndIndex > r.StartIndex {
		e2 = ie.fragments.PushBack(r.Input[r.StartIndex:r.EndIndex])
	}
	if e1 == nil {
		e1 = e2
	}
	return
}

// Called by IniParseContents; do not call directly.
func (ie *IniEditor) Section(ss IniSecStart) error {
	// git-config associates comments with following section
	e, _ := ie.appendItem(&ss.IniRange)
	ie.secEnd[ie.lastSec.String()] = e
	ie.lastSec = &ss.IniSection
	return nil
}

// Called by IniParseContents; do not call directly.
func (ie *IniEditor) Item(ii IniItem) error {
	k := ii.QKey()
	_, e := ie.appendItem(&ii.IniRange)
	ie.values[k] = append(ie.values[k], e)
	return nil
}

// Called by IniParseContents; do not call directly.
func (ie *IniEditor) Done(r IniRange) {
	e, _ := ie.appendItem(&r)
	if e == nil {
		e = ie.fragments.PushBack([]byte{})
	}
	ie.secEnd[ie.lastSec.String()] = e
	ie.lastSec = nil
}

// Create an IniEdit for a file with contents.  Note that filename is
// only used for parse errors; the file must already be read before
// calling this function.
func NewIniEdit(filename string, contents []byte) (*IniEditor, error) {
	ret := IniEditor{
		secEnd: make(map[string]*list.Element),
		values: make(map[string][]*list.Element),
	}
	err := IniParseContents(&ret, filename, contents)
	return &ret, err
}

// A bunch of edits to be applied to an INI file.
type IniEdits []func(*IniEditor)

// Delete a key.  Invoke as Del(sec, subsec, key) or Del(sec, key).
func (ie *IniEdits) Del(sec string, args...string) error {
	s, k := &IniSection{Section:sec}, ""
	switch len(args) {
	case 1:
		k = args[0]
	case 2:
		s.Subsection = &args[0]
		k = args[1]
	default:
		return ErrInvalidNumArgs
	}
	if !s.Valid() {
		return ErrInvalidSection
	}
	*ie = append(*ie, func(ie *IniEditor){ie.Del(s, k)})
	return nil
}

// Add a key, value pair.  Invoke as Add(sec, subsec, key, value) or
// Add(sec, key, value).
func (ie *IniEdits) Add(sec string, args...string) error {
	s, k, v := &IniSection{Section:sec}, "", ""
	switch len(args) {
	case 2:
		k = args[0]
		v = args[1]
	case 3:
		s.Subsection = &args[0]
		k = args[1]
		v = args[2]
	default:
		return ErrInvalidNumArgs
	}
	if !s.Valid() {
		return ErrInvalidSection
	}
	*ie = append(*ie, func(ie *IniEditor){ie.Add(s, k, v)})
	return nil
}

// Add a key, value pair.  Invoke as Set(sec, subsec, key, value) or
// Set(sec, key, value).
func (ie *IniEdits) Set(sec string, args...string) error {
	s, k, v := &IniSection{Section:sec}, "", ""
	switch len(args) {
	case 2:
		k = args[0]
		v = args[1]
	case 3:
		s.Subsection = &args[0]
		k = args[1]
		v = args[2]
	default:
		return ErrInvalidNumArgs
	}
	if !s.Valid() {
		return ErrInvalidSection
	}
	*ie = append(*ie, func(ie *IniEditor){ie.Set(s, k, v)})
	return nil
}

// Apply edits.
func (ie *IniEdits) Apply(target *IniEditor) {
	for _, f := range *ie {
		f(target)
	}
	*ie = nil
}

// A generic IniSink
type GenericIniSink struct {
	// If non-nil, only match this specific section (otherwise ignore
	// section).
	Sec *IniSection

	// Pointers to the fields that should be parsed.
	Fields map[string]interface{}

	// If no known field name is found, or if Sec does not match the
	// current section, then pass the item on to Next.
	Next IniSink
}

func (s *GenericIniSink) AddField(name string, ptr interface{}) {
	s.Fields[name] = ptr
}

func (s *GenericIniSink) String() string {
	out := strings.Builder{}
	if s.Sec != nil {
		fmt.Fprintf(&out, "%s\n", s.Sec.String())
	}
	for name, i := range s.Fields {
		v := reflect.ValueOf(i).Elem().Interface()
		fmt.Fprintf(&out, "\t%s = %s\n", name, EscapeIniValue(fmt.Sprint(v)))
	}
	return out.String()
}

func (s *GenericIniSink) Item(ii IniItem) error {
	if s.Sec.Eq(ii.IniSection) {
		if i, ok := s.Fields[ii.Key]; ok {
			v := reflect.ValueOf(i).Elem()
			if ii.Value == nil {
				v.Set(reflect.Zero(v.Type()))
			} else if v.Kind() == reflect.String {
				v.SetString(ii.Val())
			} else {
				_, err := fmt.Sscan(*ii.Value, i)
				return err
			}
			return nil
		}
	}
	if s.Next != nil {
		return s.Next.Item(ii)
	}
	return nil
}

// Make a generic IniSink that just looks an field names within a
// struct, or the ini struct field tag if one exists (similar to the
// json tag in json unmarshaling).  The returned sink does not look at
// the section, only the key and value.  If sec is non-nil, then
// ignores items whose section is not equal to sec.
func NewIniSink(sec *IniSection, i interface{}) *GenericIniSink {
	v := reflect.ValueOf(i)
	if v.Kind() != reflect.Ptr {
		return nil
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil
	}
	ret := GenericIniSink {
		Sec: sec,
		Fields: make(map[string]interface{}),
	}

	t := v.Type()
	for i, n := 0, t.NumField(); i < n; i++ {
		f := t.Field(i)
		name := f.Tag.Get("ini")
		if name == "-" {
			continue
		} else if name == "" {
			name = strings.ReplaceAll(f.Name, "_", "-")
		}
		ret.Fields[name] = v.Field(i).Addr().Interface()
	}

	return &ret
}
