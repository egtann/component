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
	"bytes"
	"fmt"
	"html/template"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
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
		name := strings.TrimSuffix(rel, ".tmpl")
		rel = path.Dir(rel)
		f, err := os.Open(fpath)
		if err != nil {
			return errors.Wrap(err, "open file")
		}
		sectionData, scopedStyle, err := splitTemplate(f)
		if err != nil {
			f.Close()
			return err
		}
		deps := map[string]bool{}
		for section, data := range sectionData {
			if len(data) == 0 {
				continue
			}
			t := compileSection(name, section, string(data), rel, deps, allNames, scopedStyle, fns)
			for _, tt := range t.Templates() {
				all.AddParseTree(tt.Tree.Name, tt.Tree)
			}
		}
		dependencies[name] = deps
		f.Close()
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "walk directory")
	}
	for name := range dependencies {
		deps := sortedDeps(name, dependencies)
		t := compileRoot(name, deps, allNames, fns)
		for _, tt := range t.Templates() {
			all.AddParseTree(tt.Tree.Name, tt.Tree)
		}
	}
	return all, nil
}

func compileSection(
	name, section, data, dir string,
	deps, all map[string]bool,
	scopedStyle bool,
	fns template.FuncMap,
) *template.Template {
	finalName := name + "#" + section
	all[finalName] = true
	t := template.Must(template.New(".<section>.").Funcs(fns).Parse(data))
	tns := getTemplateNodes(t)
	for templateNode, refName := range tns.template {
		if refName[0] == '.' {
			// external reference
			// determine absolute "path"
			refName = path.Clean(path.Join(dir, refName))
			if section == "template" {
				// if this reference is in the "template" section we'll need to
				// include the references "style" and "script" sections as well
				// so track this reference as a dependency
				deps[refName] = true
			}
			refName = refName + "#" + section
			// record the full refName so we can check later what section
			// templates were actually defined
			all[refName] = true
		} else {
			// local reference
			refName = name + "~" + refName
		}
		// rename the *parse.TemplateNode to point to the canonical name
		templateNode.Name = refName
	}
	for _, tt := range t.Templates() {
		tmplName := tt.Name()
		if tmplName == ".<section>." {
			// we used '.<section>.' as the name when compiling so it wasn't
			// considered a local template. rename it here.
			tt.Tree.Name = finalName
		} else {
			tt.Tree.Name = name + "~" + tmplName
		}
	}
	return t
}

func compileRoot(
	name string,
	deps []string,
	all map[string]bool,
	fns template.FuncMap,
) *template.Template {
	parts := map[string][]string{"style": nil, "script": nil, "template": nil}
	// check if a given template/section is available
	chk := func(name, section string) {
		if all[name+"#"+section] {
			parts[section] = append(parts[section], `{{template "`+name+"#"+section+`" .}}`)
		}
	}
	for _, dep := range deps {
		chk(dep, "style")
		chk(dep, "script")
		if dep == name {
			chk(name, "template")
		}
	}
	html := "<!DOCTYPE html>\n" +
		"<html>\n" +
		"<style>\n" + strings.Join(parts["style"], "\n") + "\n</style>\n" +
		"<script>\n" + strings.Join(parts["script"], "\n") + "\n</script>\n" +
		strings.Join(parts["template"], "\n") + "\n" +
		"</html>\n"
	return template.Must(template.New(name).Funcs(fns).Parse(html))
}

// kahn algo
func sortedDeps(name string, deps map[string]map[string]bool) []string {
	reversed, leaves := reverseDeps(name, deps)
	sorted := []string{}
	for len(leaves) > 0 {
		curr := leaves[0]
		leaves = leaves[1:]
		sorted = append(sorted, curr)
		idx := len(leaves)
		for dep := range reversed[curr] {
			add := true
			for n, m := range reversed {
				if n != curr && m[dep] {
					// edges remain
					add = false
					break
				}
			}
			if add {
				leaves = append(leaves, dep)
			}
		}
		delete(reversed, curr)
		if len(leaves) != idx {
			sort.Strings(leaves[idx:])
		}
	}
	if len(reversed) > 0 {
		panic("cycles")
	}
	return sorted
}

func reverseDeps(
	name string,
	deps map[string]map[string]bool,
) (map[string]map[string]bool, []string) {
	reversed := map[string]map[string]bool{}
	parents := []string{name}
	processed := map[string]bool{}
	leaves := []string{}
	var parent string
	for len(parents) > 0 {
		parent, parents = parents[0], parents[1:]
		processed[parent] = true
		if len(deps[parent]) == 0 {
			leaves = append(leaves, parent)
		}
		for dep := range deps[parent] {
			if _, ok := reversed[dep]; !ok {
				reversed[dep] = map[string]bool{}
			}
			reversed[dep][parent] = true
			if !processed[dep] {
				parents = append(parents, dep)
			}
		}
	}
	sort.Strings(leaves)
	return reversed, leaves
}

func expandDependencies(
	name, chk string,
	dependencies map[string]map[string]bool,
) {
	for dep := range dependencies[chk] {
		if _, ok := dependencies[name][dep]; !ok {
			dependencies[name][dep] = true
			expandDependencies(name, dep, dependencies)
		}
	}
}

func splitTemplate(r io.Reader) (map[string][]byte, bool, error) {
	z := html.NewTokenizer(r)
	cur := ""
	sections := map[string][]byte{"script": nil, "style": nil, "template": nil}
	depth := 0
	scopedStyle := false
	for t := z.Next(); t != html.ErrorToken; t = z.Next() {
		tn, _ := z.TagName()
		if _, ok := sections[string(tn)]; ok {
			if t == html.StartTagToken {
				if string(tn) == "style" {
					k, _, a := z.TagAttr()
					for {
						if string(k) == "scoped" {
							scopedStyle = true
							break
						}
						if !a {
							break
						}
						k, _, a = z.TagAttr()
					}
				}

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
		return nil, false, err
	}
	for s, d := range sections {
		d = bytes.Trim(d, "\n")
		diff := len(d) - len(bytes.TrimLeft(d, " \t"))
		if diff > 0 {
			pfx := d[:diff]
			lines := bytes.Split(d, []byte{'\n'})
			for i, line := range lines {
				lines[i] = bytes.TrimPrefix(line, pfx)
			}
			d = bytes.Join(lines, []byte{'\n'})
		}
		sections[s] = d
	}
	return sections, scopedStyle, nil
}

func getTemplateNodes(t *template.Template) *tnodes {
	tns := &tnodes{template: map[*parse.TemplateNode]string{}}
	tns.checkListNode(t.Tree.Root)
	return tns
}

type tnodes struct {
	template map[*parse.TemplateNode]string
	text     []*parse.TextNode
}

func (tns *tnodes) checkListNode(ln *parse.ListNode) {
	if ln == nil || len(ln.Nodes) == 0 {
		return
	}
	for _, n := range ln.Nodes {
		tns.checkNode(n)
	}
}
func (tns *tnodes) checkPipeNode(pn *parse.PipeNode) {
	if pn == nil || len(pn.Cmds) == 0 {
		return
	}
	for _, cn := range pn.Cmds {
		tns.checkCommandNode(cn)
	}
}
func (tns *tnodes) checkCommandNode(cn *parse.CommandNode) {
	if cn == nil || len(cn.Args) == 0 {
		return
	}
	for _, n := range cn.Args {
		tns.checkNode(n)
	}
}

func (tns *tnodes) checkNodeSlice(nodes []parse.Node) {
	if len(nodes) == 0 {
		return
	}
	for _, n := range nodes {
		tns.checkNode(n)
	}
}

func (tns *tnodes) checkNode(n parse.Node) {
	switch t := n.(type) {
	case *parse.ActionNode:
		tns.checkPipeNode(t.Pipe)
	case *parse.BranchNode:
		tns.checkPipeNode(t.Pipe)
		tns.checkListNode(t.List)
		tns.checkListNode(t.ElseList)
	case *parse.RangeNode:
		tns.checkPipeNode(t.Pipe)
		tns.checkListNode(t.List)
		tns.checkListNode(t.ElseList)
	case *parse.WithNode:
		tns.checkPipeNode(t.Pipe)
		tns.checkListNode(t.List)
		tns.checkListNode(t.ElseList)
	case *parse.IfNode:
		tns.checkPipeNode(t.Pipe)
		tns.checkListNode(t.List)
		tns.checkListNode(t.ElseList)
	case *parse.ChainNode:
		tns.checkNode(t.Node)
	case *parse.CommandNode:
		tns.checkCommandNode(t)
	case *parse.ListNode:
		tns.checkListNode(t)
	case *parse.PipeNode:
		tns.checkPipeNode(t)
	case *parse.TemplateNode:
		tns.template[t] = t.Name
	case *parse.TextNode:
		tns.text = append(tns.text, t)
	}
}
