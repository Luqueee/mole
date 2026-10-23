package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
)

var errPickCancelled = errors.New("cancelled")

// pickFiles shows a multi-select of the regular files in dir and returns the
// chosen paths. Hidden files are listed only when all is set.
func pickFiles(dir string, all bool) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type item struct {
		name string
		size int64
	}
	var items []item
	for _, e := range ents {
		if !all && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, e.Name())) // follows symlinks
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		items = append(items, item{e.Name(), st.Size()})
	}
	if len(items) == 0 {
		return nil, errors.New("there are no files in this folder")
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].name) < strings.ToLower(items[j].name) })

	opts := make([]huh.Option[string], len(items))
	for i, it := range items {
		opts[i] = huh.NewOption(fmt.Sprintf("%s  (%s)", it.name, humanSize(it.size)), filepath.Join(dir, it.name))
	}
	var picked []string
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Send which files?").
			Description("space: select · ctrl+a: all · /: filter · enter: send").
			Options(opts...).
			Filterable(true).
			Height(min(len(opts)+3, 18)).
			Value(&picked),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, errPickCancelled
		}
		return nil, err
	}
	if len(picked) == 0 {
		return nil, errPickCancelled
	}
	return picked, nil
}
