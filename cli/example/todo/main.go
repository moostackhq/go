// Command todo is a small JSON-backed todo-list CLI built on
// github.com/moostackhq/go/cli. It exists to show the library in
// realistic use (subcommands, inherited flags, validators,
// repeatable flags, time parsing, and friendly error/help
// rendering) without being a tutorial slog.
//
// Build & run:
//
//	go run ./example/todo --help
//	go run ./example/todo add "buy milk" --priority high --tag groceries
//	go run ./example/todo list --status open
//	go run ./example/todo done 1
//	go run ./example/todo clear --confirm
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/moostackhq/go/cli"
	"github.com/moostackhq/go/cli/middleware"
)

// ---------- model ----------

type Item struct {
	ID       int       `json:"id"`
	Text     string    `json:"text"`
	Priority string    `json:"priority,omitempty"`
	Tags     []string  `json:"tags,omitempty"`
	Due      time.Time `json:"due,omitempty"`
	Done     bool      `json:"done,omitempty"`
	Created  time.Time `json:"created"`
}

type store struct {
	Items  []Item `json:"items"`
	NextID int    `json:"next_id"`
}

func loadStore(path string) (*store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(data) == 0) {
		// Missing file or empty file (e.g. `touch ~/.todo.json`)
		// both mean "no items yet."
		return &store{NextID: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.NextID == 0 {
		s.NextID = 1
	}
	return &s, nil
}

func (s *store) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}

// ---------- shared (root flags + the id positional) ----------

// Defaults for `--file` come from $TODO_FILE first, then
// $HOME/.todo.json, then ./todo.json. Computed at package init so
// the value rendered in --help is the user's actual default.
var defaultFile = func() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".todo.json")
	}
	return "todo.json"
}()

var (
	fileFlag = cli.StringFlag("file").
			Default(defaultFile).
			Env("TODO_FILE").
			Help("path to the JSON-backed todo store")

	formatFlag = cli.StringFlag("format").
			Default("text").
			OneOf("text", "json").
			Help("output format")

	// done / rm / show all take the same positional.
	idArg = cli.IntArg("id").
		Required().
		Help("numeric id from `todo list`")
)

// ---------- add ----------

var (
	addText = cli.StringArg("description").
		Required().
		Help("what the todo is about")

	addPriority = cli.StringFlag("priority").
			Short('p').
			Default("medium").
			OneOf("high", "medium", "low").
			Help("item priority")

	addTags = cli.StringSliceFlag("tag").
		Short('t').
		Help("tag (may be repeated)")

	addDue = cli.TimeFlag("due").
		Help("due date, RFC3339 (e.g. 2026-06-01T17:00:00Z)")
)

var addCmd = &cli.Command{
	Name:  "add",
	Help:  "add a new todo",
	Flags: cli.Flags(addPriority, addTags, addDue),
	Args:  cli.Args(addText),
	Examples: []cli.Example{
		{Cmd: `todo add "buy milk" --priority high --tag groceries`,
			Help: "add a high-priority item with a tag"},
	},
	Run: func(ctx cli.Context) error {
		s, err := loadStore(fileFlag.Get(ctx))
		if err != nil {
			return err
		}
		item := Item{
			ID:       s.NextID,
			Text:     addText.Get(ctx),
			Priority: addPriority.Get(ctx),
			Tags:     addTags.Get(ctx),
			Created:  time.Now().UTC(),
		}
		if d, ok := addDue.Lookup(ctx); ok {
			item.Due = d
		}
		s.Items = append(s.Items, item)
		s.NextID++
		if err := s.save(fileFlag.Get(ctx)); err != nil {
			return err
		}
		return emitItem(ctx, item)
	},
}

// ---------- list ----------

var (
	listStatus = cli.StringFlag("status").
			Short('s').
			Default("open").
			OneOf("open", "done", "all").
			Help("show items in this status")

	listTag = cli.StringFlag("with-tag").
		Help("only items carrying this tag")
)

var listCmd = &cli.Command{
	Name:  "list",
	Help:  "list todos",
	Flags: cli.Flags(listStatus, listTag),
	Run: func(ctx cli.Context) error {
		s, err := loadStore(fileFlag.Get(ctx))
		if err != nil {
			return err
		}
		items := filterItems(s.Items, listStatus.Get(ctx), listTag.Get(ctx))
		return emitItems(ctx, items)
	},
}

// ---------- done ----------

var doneCmd = &cli.Command{
	Name: "done",
	Help: "mark a todo as completed",
	Args: cli.Args(idArg),
	Run: func(ctx cli.Context) error {
		s, err := loadStore(fileFlag.Get(ctx))
		if err != nil {
			return err
		}
		id := idArg.Get(ctx)
		for i := range s.Items {
			if s.Items[i].ID == id {
				s.Items[i].Done = true
				if err := s.save(fileFlag.Get(ctx)); err != nil {
					return err
				}
				return emitItem(ctx, s.Items[i])
			}
		}
		return cli.UsageError("no todo with id %d", id)
	},
}

// ---------- rm ----------

var rmCmd = &cli.Command{
	Name: "rm",
	Help: "delete a todo",
	Args: cli.Args(idArg),
	Run: func(ctx cli.Context) error {
		s, err := loadStore(fileFlag.Get(ctx))
		if err != nil {
			return err
		}
		id := idArg.Get(ctx)
		for i, item := range s.Items {
			if item.ID == id {
				s.Items = slices.Delete(s.Items, i, i+1)
				if err := s.save(fileFlag.Get(ctx)); err != nil {
					return err
				}
				fmt.Fprintf(ctx.Stdout(), "deleted #%d\n", id)
				return nil
			}
		}
		return cli.UsageError("no todo with id %d", id)
	},
}

// ---------- show ----------

var showCmd = &cli.Command{
	Name: "show",
	Help: "show a single todo",
	Args: cli.Args(idArg),
	Run: func(ctx cli.Context) error {
		s, err := loadStore(fileFlag.Get(ctx))
		if err != nil {
			return err
		}
		id := idArg.Get(ctx)
		for _, item := range s.Items {
			if item.ID == id {
				return emitItem(ctx, item)
			}
		}
		return cli.UsageError("no todo with id %d", id)
	},
}

// ---------- clear ----------

var clearConfirm = cli.BoolFlag("confirm").
	Help("required to actually delete completed items")

var clearCmd = &cli.Command{
	Name:  "clear",
	Help:  "delete every completed todo",
	Flags: cli.Flags(clearConfirm),
	Run: func(ctx cli.Context) error {
		if !clearConfirm.Get(ctx) {
			return cli.UsageError("pass --confirm to actually delete")
		}
		s, err := loadStore(fileFlag.Get(ctx))
		if err != nil {
			return err
		}
		before := len(s.Items)
		s.Items = slices.DeleteFunc(s.Items, func(it Item) bool { return it.Done })
		if err := s.save(fileFlag.Get(ctx)); err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout(), "deleted %d completed item(s)\n", before-len(s.Items))
		return nil
	},
}

// ---------- root ----------

var root = &cli.Command{
	Name:    "todo",
	Version: "0.1.0",
	Help:    "personal todo list, stored as JSON on disk",
	Long: "todo is a tiny JSON-backed todo CLI. State lives in a single " +
		"file (--file, default $HOME/.todo.json or $TODO_FILE if set); " +
		"every subcommand reads and writes that file directly.",
	Flags:       cli.Flags(fileFlag, formatFlag),
	Use:         []cli.Middleware{middleware.Recover()},
	Subcommands: []*cli.Command{addCmd, listCmd, doneCmd, rmCmd, showCmd, clearCmd},
}

func main() {
	os.Exit(root.Exec(os.Args[1:]))
}

// ---------- rendering ----------

func emitItem(ctx cli.Context, item Item) error {
	if formatFlag.Get(ctx) == "json" {
		return json.NewEncoder(ctx.Stdout()).Encode(item)
	}
	mark := " "
	if item.Done {
		mark = "x"
	}
	fmt.Fprintf(ctx.Stdout(), "[%s] #%d %s\n", mark, item.ID, item.Text)
	if item.Priority != "" {
		fmt.Fprintf(ctx.Stdout(), "    priority: %s\n", item.Priority)
	}
	if len(item.Tags) > 0 {
		fmt.Fprintf(ctx.Stdout(), "    tags: %s\n", strings.Join(item.Tags, ", "))
	}
	if !item.Due.IsZero() {
		fmt.Fprintf(ctx.Stdout(), "    due: %s\n", item.Due.Format(time.RFC3339))
	}
	return nil
}

func emitItems(ctx cli.Context, items []Item) error {
	if formatFlag.Get(ctx) == "json" {
		// Encode the slice as-is so json output is always a list,
		// even when empty.
		return json.NewEncoder(ctx.Stdout()).Encode(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(ctx.Stdout(), "(no items)")
		return nil
	}
	for _, item := range items {
		mark := " "
		if item.Done {
			mark = "x"
		}
		line := fmt.Sprintf("[%s] #%-3d %s", mark, item.ID, item.Text)
		var annot []string
		if item.Priority != "" && item.Priority != "medium" {
			annot = append(annot, item.Priority)
		}
		if len(item.Tags) > 0 {
			annot = append(annot, "#"+strings.Join(item.Tags, " #"))
		}
		if !item.Due.IsZero() {
			annot = append(annot, "due "+item.Due.Format("2006-01-02"))
		}
		if len(annot) > 0 {
			line += "  (" + strings.Join(annot, " · ") + ")"
		}
		fmt.Fprintln(ctx.Stdout(), line)
	}
	return nil
}

func filterItems(items []Item, status, tag string) []Item {
	out := make([]Item, 0, len(items))
	for _, item := range items {
		switch status {
		case "open":
			if item.Done {
				continue
			}
		case "done":
			if !item.Done {
				continue
			}
		}
		if tag != "" && !slices.Contains(item.Tags, tag) {
			continue
		}
		out = append(out, item)
	}
	return out
}
