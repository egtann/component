// package component generates HTML templates from single-file components,
// similar to Vue.js or Svbtle. Single-file components contain the style,
// scripts, and structure to render a given component, rather than placing
// styles and scripts in separate directories.
//
// Go's stdlib and HTML templates get us 90% of the way to single-file
// components, but they have a drawback preventing their use for this purpose:
// since single-file components embed style and script tags, including the same
// template 100 times (such as for an item in a list) would include the
// associated script and style tags 100 times and create tons of bloat.
// Instead, for any template included as a partial, this package ensures only 1
// copy of its script and style tags are included. For example, if the same
// template is included twice, component excludes the second one's duplicated
// style and script tags.
//
// To prevent namespace collisions, you should namespace each of your styles
// and Javascript functions under a name matching the component, however this
// is not enforced by the package.
package component

import (
	"fmt"
	"html/template"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template/parse"

	"github.com/pkg/errors"
	"golang.org/x/net/html"
)

// CompileDir recursively walks the given directory to compile component
// templates, which are identified by the ".tmpl" extension.
//
// Components may only have <style>, <script>, and <template> root tags. The
// structure of the component, e.g. the text and divs that make it up, should
// go in the <template> tag.
//
// To use the returned template, or render a specific page, simply call:
//
//	err := t.ExecuteTemplate(out, "./homepage", nil).
//
// Note that we use a relative path with a forward slash (on any platform --
// even on Windows). Names for templates are defined automatically based on the
// name of the file it was drawn from. e.g. assume we have one file named
// "templates/analytics.tmpl" and another named "templates/graphs/user.tmpl":
//
//	// main.go
//	t, err := component.CompileDir("templates")
//	if err != nil {
//		return err
//	}
//	err = t.ExecuteTemplate(out, "./analytics", nil)
//	// Or...
//	err = t.ExecuteTemplate(out, "./graphs/user", nil)
//
// to render our analytics page on its own. Rendering one template within
// another as a partial stays the same, although we use a relative path, such
// as in the following example:
//
//	// analytics.tmpl
//	<template>
//		<h1>Analytics</h1>
//		{{ template "./analytics/graphs/users" . }}
//	</template>
//
// Again, note the leading "./" in the path.
//
// You can also define and re-use templates locally within a component. For
// locally defined templates only used within a single component, do not
// prepend "./", e.g.:
//
//	// analytics.tmpl
//	{{ define "local" }}<p>Local Template!</p>{{ end }}
//	<template>
//		<h1>Analytics</h1>
//		{{ template "local" }}
//	</template>
//
// You'll find more examples in the package's templates/ directory.
func CompileDir(
	dirname string,
	fns template.FuncMap,
) (*template.Template, error) {
	all := template.New("").Funcs(fns)
	dependencies := map[string]map[string]bool{}
	allNames := map[string]bool{}
	err := filepath.Walk(dirname, func(fpath string, info os.FileInfo, err error) error {
		if info == nil {
			return fmt.Errorf("%s does not exist", fpath)
		}
		if info.IsDir() || !strings.HasSuffix(fpath, ".tmpl") {
			return nil
		}
		rel, err := filepath.Rel(dirname, fpath)
		if err != nil {
			return errors.Wrap(err, "filepath rel")
		}
		rel = strings.Replace(rel, string(os.PathSeparator), "/", -1)
		name := "./" + strings.TrimSuffix(rel, ".tmpl")
		f, err := os.Open(fpath)
		if err != nil {
			return errors.Wrap(err, "open file")
		}
		sectionData, err := splitTemplate(f)
		if err != nil {
			f.Close()
			return errors.Wrap(err, "split template")
		}
		deps := map[string]bool{}
		for k, v := range sectionData {
			if len(v) == 0 {
				continue
			}
			t := template.Must(template.New(name + "-" + k).Funcs(fns).Parse(string(v)))
			allNames[name+"-"+k] = true
			for tn, dep := range getDependencies(t) {
				var depName string
				if dep[0] == '.' {
					depName = path.Clean(path.Join(path.Dir(rel), dep))
					depName = "./" + depName
					if k == "template" {
						deps[depName] = true
					}
					depName = depName + "-" + k
					allNames[depName] = true
				} else {
					depName = name + "__" + dep
				}
				tn.Name = depName
			}
			for _, tt := range t.Templates() {
				depName := tt.Name()
				if depName[0] != '.' {
					depName = name + "__" + depName
				}
				all.AddParseTree(depName, tt.Tree)
			}
		}
		dependencies[name] = deps
		f.Close()
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "walk directory")
	}
	for name, deps := range dependencies {
		for chk := range deps {
			expandDependencies(name, chk, dependencies)
		}
		parts := map[string][]string{}
		if allNames[name+"-style"] {
			parts["style"] = append(parts["style"], `{{template "`+name+`-style" . }}`)
		}
		if allNames[name+"-script"] {
			parts["script"] = append(parts["script"], `{{template "`+name+`-script" . }}`)
		}
		if ok, exists := allNames[name+"-template"]; ok && exists {
			parts["template"] = append(parts["template"], `{{template "`+name+`-template" .}}`)
		}
		depList := []string{}
		for dep := range deps {
			depList = append(depList, dep)
			if allNames[dep+"-style"] {
				parts["style"] = append(parts["style"], `{{template "`+dep+`-style" .}}`)
			}
			if allNames[dep+"-script"] {
				parts["script"] = append(parts["script"], `{{template "`+dep+`-script" .}}`)
			}
		}
		html := "<html>\n" +
			"<style>\n" + strings.Join(parts["style"], "\n") + "\n</style>\n" +
			"<script>\n" + strings.Join(parts["script"], "\n") + "\n</script>\n" +
			strings.Join(parts["template"], "\n") + "\n" +
			"</html>"
		t := template.Must(template.New(name).Funcs(fns).Parse(html))
		all.AddParseTree(name, t.Tree)
	}
	return all, nil
}

func expandDependencies(name, chk string, dependencies map[string]map[string]bool) {
	for dep := range dependencies[chk] {
		if _, ok := dependencies[name][dep]; !ok {
			dependencies[name][dep] = true
			expandDependencies(name, dep, dependencies)
		}
	}
}

func splitTemplate(r io.Reader) (map[string][]byte, error) {
	z := html.NewTokenizer(r)
	cur := ""
	sections := map[string][]byte{
		"script":   nil,
		"style":    nil,
		"template": nil,
	}
	depth := 0
	for t := z.Next(); t != html.ErrorToken; t = z.Next() {
		tn, _ := z.TagName()
		if _, ok := sections[string(tn)]; ok {
			if t == html.StartTagToken {
				depth++
				if depth == 1 {
					cur = string(tn)
					continue
				}
			} else if t == html.EndTagToken {
				depth--
				if depth == 0 {
					cur = ""
					continue
				}
			}
		}
		if cur != "" {
			sections[cur] = append(sections[cur], z.Raw()...)
		}
	}
	if err := z.Err(); err != io.EOF {
		return nil, err
	}
	return sections, nil
}

func getDependencies(t *template.Template) map[*parse.TemplateNode]string {
	deps := map[*parse.TemplateNode]string{}
	checkListNode(t.Tree.Root, deps)
	return deps
}

func checkListNode(ln *parse.ListNode, deps map[*parse.TemplateNode]string) {
	if ln == nil || len(ln.Nodes) == 0 {
		return
	}
	for _, n := range ln.Nodes {
		checkNode(n, deps)
	}
}

func checkPipeNode(pn *parse.PipeNode, deps map[*parse.TemplateNode]string) {
	if pn == nil || len(pn.Cmds) == 0 {
		return
	}
	for _, cn := range pn.Cmds {
		checkCommandNode(cn, deps)
	}
}

func checkCommandNode(cn *parse.CommandNode, deps map[*parse.TemplateNode]string) {
	if cn == nil || len(cn.Args) == 0 {
		return
	}
	for _, n := range cn.Args {
		checkNode(n, deps)
	}
}

func checkNodeSlice(nodes []parse.Node, deps map[*parse.TemplateNode]string) {
	if len(nodes) == 0 {
		return
	}
	for _, n := range nodes {
		checkNode(n, deps)
	}
}

func checkNode(n parse.Node, deps map[*parse.TemplateNode]string) {
	switch t := n.(type) {
	case *parse.ActionNode:
		checkPipeNode(t.Pipe, deps)
	case *parse.BranchNode:
		checkPipeNode(t.Pipe, deps)
		checkListNode(t.List, deps)
		checkListNode(t.ElseList, deps)
	case *parse.RangeNode:
		checkPipeNode(t.Pipe, deps)
		checkListNode(t.List, deps)
		checkListNode(t.ElseList, deps)
	case *parse.WithNode:
		checkPipeNode(t.Pipe, deps)
		checkListNode(t.List, deps)
		checkListNode(t.ElseList, deps)
	case *parse.IfNode:
		checkPipeNode(t.Pipe, deps)
		checkListNode(t.List, deps)
		checkListNode(t.ElseList, deps)
	case *parse.ChainNode:
		checkNode(t.Node, deps)
	case *parse.CommandNode:
		checkCommandNode(t, deps)
	case *parse.ListNode:
		checkListNode(t, deps)
	case *parse.PipeNode:
		checkPipeNode(t, deps)
	case *parse.TemplateNode:
		deps[t] = t.Name
	}
}
