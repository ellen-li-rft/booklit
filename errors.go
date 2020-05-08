package booklit

import (
	"bufio"
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/segmentio/textio"
	"github.com/vito/booklit/ast"
	"github.com/vito/booklit/errhtml"
)

var errorTmpl *template.Template

func init() {
	errorTmpl = template.New("errors").Funcs(template.FuncMap{
		"error": func(err error) (template.HTML, error) {
			buf := new(bytes.Buffer)
			if prettyErr, ok := err.(PrettyError); ok {
				renderErr := prettyErr.PrettyHTML(buf)
				if renderErr != nil {
					return "", renderErr
				}

				return template.HTML(buf.String()), nil
			} else {
				return template.HTML(
					`<pre class="raw-error">` +
						template.HTMLEscapeString(err.Error()) +
						`</pre>`,
				), nil
			}
		},
		"annotate": func(loc ErrorLocation) (template.HTML, error) {
			buf := new(bytes.Buffer)
			err := loc.AnnotatedHTML(buf)
			if err != nil {
				return "", err
			}

			return template.HTML(buf.String()), nil
		},
	})

	for _, asset := range errhtml.AssetNames() {
		info, err := errhtml.AssetInfo(asset)
		if err != nil {
			panic(err)
		}

		content := strings.TrimRight(string(errhtml.MustAsset(asset)), "\n")

		_, err = errorTmpl.New(filepath.Base(info.Name())).Parse(content)
		if err != nil {
			panic(err)
		}
	}
}

func ErrorPage(err error, w http.ResponseWriter) {
	renderErr := errorTmpl.Lookup("page.tmpl").Execute(w, err)
	if renderErr != nil {
		fmt.Fprintf(w, "failed to render error page: %s", renderErr)
	}
}

type PrettyError interface {
	PrettyPrint(io.Writer)
	PrettyHTML(io.Writer) error
}

type ParseError struct {
	Err error

	ErrorLocation
}

func (err ParseError) Error() string {
	return err.Err.Error()
}

func (err ParseError) PrettyPrint(out io.Writer) {
	fmt.Fprintf(out, err.Annotate("parse error: %s\n\n", err.Err))
	err.AnnotateLocation(out)
}

func (err ParseError) PrettyHTML(out io.Writer) error {
	return errorTmpl.Lookup("parse-error.tmpl").Execute(out, err)
}

type UnknownTagError struct {
	TagName string

	ErrorLocation
}

func (err UnknownTagError) Error() string {
	return fmt.Sprintf("unknown tag '%s'", err.TagName)
}

func (err UnknownTagError) PrettyPrint(out io.Writer) {
	fmt.Fprintf(out, err.Annotate("reference points to unknown tag '%s':\n\n", err.TagName))
	err.AnnotateLocation(out)
}

func (err UnknownTagError) PrettyHTML(out io.Writer) error {
	return errorTmpl.Lookup("unknown-tag.tmpl").Execute(out, err)
}

type AmbiguousReferenceError struct {
	TagName          string
	DefinedLocations []ErrorLocation

	ErrorLocation
}

func (err AmbiguousReferenceError) Error() string {
	return fmt.Sprintf(
		"ambiguous target for tag '%s'",
		err.TagName,
	)
}

func (err AmbiguousReferenceError) PrettyPrint(out io.Writer) {
	fmt.Fprintf(out, err.Annotate("%s:\n\n", err))
	err.AnnotateLocation(out)
	fmt.Fprintf(out, "the same tag was defined in the following locations:\n\n")

	for _, loc := range err.DefinedLocations {
		fmt.Fprintf(out, "- %s:\n", loc.FilePath)
		loc.AnnotateLocation(textio.NewPrefixWriter(out, "  "))
	}

	fmt.Fprintf(out, "one of these must be changed.\n")
}

func (err AmbiguousReferenceError) PrettyHTML(out io.Writer) error {
	return errorTmpl.Lookup("ambiguous-reference.tmpl").Execute(out, err)
}

type UndefinedFunctionError struct {
	Function string

	ErrorLocation
}

func (err UndefinedFunctionError) Error() string {
	return fmt.Sprintf(
		"undefined function \\%s",
		err.Function,
	)
}

func (err UndefinedFunctionError) PrettyPrint(out io.Writer) {
	fmt.Fprintf(out, err.Annotate("%s:\n\n", err))
	err.AnnotateLocation(out)
}

func (err UndefinedFunctionError) PrettyHTML(out io.Writer) error {
	return errorTmpl.Lookup("undefined-function.tmpl").Execute(out, err)
}

type FailedFunctionError struct {
	Function string
	Err      error

	ErrorLocation
}

func (err FailedFunctionError) Error() string {
	return fmt.Sprintf(
		"function \\%s returned an error: %s",
		err.Function,
		err.Err,
	)
}

func (err FailedFunctionError) PrettyPrint(out io.Writer) {
	fmt.Fprintf(out, err.Annotate("function \\%s returned an error:\n\n", err.Function))
	err.AnnotateLocation(out)
	fmt.Fprintf(out, "error: %s\n", err.Err)
}

func (err FailedFunctionError) PrettyHTML(out io.Writer) error {
	return errorTmpl.Lookup("function-error.tmpl").Execute(out, err)
}

type ErrorLocation struct {
	FilePath     string
	NodeLocation ast.Location
	Length       int
}

func (loc ErrorLocation) Annotate(msg string, args ...interface{}) string {
	if loc.NodeLocation.Line == 0 {
		return fmt.Sprintf("%s: %s", loc.FilePath, fmt.Sprintf(msg, args...))
	} else {
		return fmt.Sprintf("%s:%d: %s", loc.FilePath, loc.NodeLocation.Line, fmt.Sprintf(msg, args...))
	}
}

func (loc ErrorLocation) AnnotateLocation(out io.Writer) error {
	if loc.NodeLocation.Line == 0 {
		// location unavailable
		return nil
	}

	line, err := loc.lineInQuestion()
	if err != nil {
		return err
	}

	prefix := fmt.Sprintf("% 4d| ", loc.NodeLocation.Line)

	_, err = fmt.Fprintf(out, "%s%s\n", prefix, line)
	if err != nil {
		return err
	}

	pad := strings.Repeat(" ", len(prefix)+loc.NodeLocation.Col-1)
	_, err = fmt.Fprintf(out, "%s\x1b[31m%s\x1b[0m\n", pad, strings.Repeat("^", loc.Length))
	if err != nil {
		return err
	}

	return nil
}

type AnnotationData struct {
	FilePath                  string
	EOF                       bool
	Lineno                    string
	Prefix, Annotated, Suffix string
}

func (loc ErrorLocation) AnnotatedHTML(out io.Writer) error {
	if loc.NodeLocation.Line == 0 {
		// location unavailable
		return nil
	}

	line, err := loc.lineInQuestion()
	if err != nil {
		return err
	}

	data := AnnotationData{
		FilePath: loc.FilePath,
		Lineno:   fmt.Sprintf("% 4d", loc.NodeLocation.Line),
	}

	if line == "" {
		data.EOF = true
	}

	offset := loc.NodeLocation.Col - 1
	if len(line) >= offset+loc.Length {
		data.Prefix = line[0:offset]
		data.Annotated = line[offset : offset+loc.Length]
		data.Suffix = line[offset+loc.Length:]
	}

	return errorTmpl.Lookup("annotated-line.tmpl").Execute(out, data)
}

func (loc ErrorLocation) lineInQuestion() (string, error) {
	file, err := os.Open(loc.FilePath)
	if err != nil {
		return "", err
	}

	defer file.Close()

	buf := bufio.NewReader(file)

	for i := 0; i < loc.NodeLocation.Line-1; i++ {
		_, _, err := buf.ReadLine()
		if err != nil {
			return "", err
		}
	}

	lineInQuestion, _, err := buf.ReadLine()
	if err != nil {
		if err == io.EOF {
			return "", nil
		}

		return "", err
	}

	return string(lineInQuestion), nil
}