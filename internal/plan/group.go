// Package plan decides what goes on which volume.
//
// It works in two steps. Group turns the flat write set into items — subtrees
// small enough to be worth keeping together, and individual files when they are
// not — and Pack places those items onto fixed-capacity volumes. Neither step
// knows anything about the medium beyond a byte count.
package plan

import (
	"path/filepath"
	"sort"
	"strings"

	"kura/internal/scan"
)

// Item is the unit the packer places on a volume.
//
// Keeping a directory together on one volume matters for restores: pulling back
// a season folder or a BDMV should not mean mounting three cartridges. So an
// item is a whole subtree when the subtree is small enough to be worth holding
// together, and an individual file only when it is not.
type Item struct {
	Name string `json:"name"`

	// Size is what the item occupies on a volume, container framing included,
	// because that is the number the packer has to fit.
	Size  int64 `json:"size"`
	Count int   `json:"files"`
	Dir   bool  `json:"dir"`

	// Files is everything in the item; Loose is the part written as objects of
	// their own and Containers the rest, already assigned. None of it survives
	// into the plan file — Entries carries the result — so a writer reading a
	// plan back works from Entries alone.
	Files      []*scan.File `json:"-"`
	Loose      []*scan.File `json:"-"`
	Containers []Container  `json:"-"`
}

type node struct {
	name     string
	children map[string]*node
	files    []*scan.File
	size     int64
}

func newNode(name string) *node {
	return &node{name: name, children: make(map[string]*node)}
}

func (n *node) insert(parts []string, f *scan.File) {
	n.size += f.Size
	if len(parts) == 0 {
		n.files = append(n.files, f)
		return
	}
	c, ok := n.children[parts[0]]
	if !ok {
		c = newNode(parts[0])
		n.children[parts[0]] = c
	}
	c.insert(parts[1:], f)
}

// Group turns a flat file list into packable items.
//
// A subtree becomes one atomic item when it fits within maxGroup. Larger
// subtrees are broken up: their files and child directories are gathered into
// runs of at most maxGroup, in name order, and only children that individually
// exceed the cap are descended into.
//
// The cap is measured in source bytes, because grouping is a locality decision
// about what a restore has to mount together. Each item's Size then comes back
// in stored bytes, because packing is a capacity decision.
func Group(files []*scan.File, maxGroup int64, lay Layout) []Item {
	if len(files) == 0 {
		return nil
	}
	root := newNode("")
	for _, f := range files {
		dir := filepath.Dir(f.Path)
		parts := strings.Split(strings.Trim(dir, "/"), "/")
		if len(parts) == 1 && parts[0] == "" {
			parts = nil
		}
		root.insert(parts, f)
	}

	var items []Item
	emit(root, "", maxGroup, lay, &items)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

func emit(n *node, path string, maxGroup int64, lay Layout, out *[]Item) {
	if n.size == 0 {
		return
	}
	if n.size <= maxGroup {
		files := make([]*scan.File, 0, 16)
		collect(n, &files)
		if len(files) > 0 {
			*out = append(*out, lay.makeItem(path+"/", files, true))
		}
		return
	}

	// Too large to hold together. Chunk the files living directly here...
	if len(n.files) > 0 {
		direct := append([]*scan.File(nil), n.files...)
		sort.Slice(direct, func(i, j int) bool { return direct[i].Path < direct[j].Path })
		var run []*scan.File
		var runSize int64
		flush := func() {
			if len(run) == 0 {
				return
			}
			name := run[0].Path
			dir := len(run) > 1
			if dir {
				name = path + "/[" + filepath.Base(run[0].Path) + " .. " + filepath.Base(run[len(run)-1].Path) + "]"
			}
			*out = append(*out, lay.makeItem(name, run, dir))
			run, runSize = nil, 0
		}
		for _, f := range direct {
			if runSize+f.Size > maxGroup && len(run) > 0 {
				flush()
			}
			if f.Size > maxGroup {
				*out = append(*out, lay.makeItem(f.Path, []*scan.File{f}, false))
				continue
			}
			run = append(run, f)
			runSize += f.Size
		}
		flush()
	}

	// ...then the subdirectories, merging consecutive siblings that fit.
	//
	// The merge is not optional. Emitting one item per child instead means a
	// fanout directory — immich's hash tree is the real example — produces an
	// item per leaf: a first attempt over the real archive gave 107,051 of
	// them, 99,492 under 10 MB. Merging brings it to 4,298 (DESIGN §4.1). Only
	// children that individually exceed the cap are descended into.
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)

	var run []*node
	var runSize int64
	flushRun := func() {
		if len(run) == 0 {
			return
		}
		files := make([]*scan.File, 0, 16)
		for _, c := range run {
			collect(c, &files)
		}
		// A run of one keeps the plain subtree name, so merging changes nothing
		// for a tree whose children each fill the cap on their own.
		name := path + "/" + run[0].name + "/"
		if len(run) > 1 {
			name = path + "/[" + run[0].name + " .. " + run[len(run)-1].name + "]/"
		}
		*out = append(*out, lay.makeItem(name, files, true))
		run, runSize = nil, 0
	}
	for _, name := range names {
		c := n.children[name]
		if c.size == 0 {
			continue
		}
		if c.size > maxGroup {
			// Descending breaks the run: the siblings on either side are no
			// longer consecutive, and a name claiming to span them would lie.
			flushRun()
			emit(c, path+"/"+name, maxGroup, lay, out)
			continue
		}
		if runSize+c.size > maxGroup {
			flushRun()
		}
		run = append(run, c)
		runSize += c.size
	}
	flushRun()
}

func collect(n *node, out *[]*scan.File) {
	*out = append(*out, n.files...)
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		collect(n.children[name], out)
	}
}
