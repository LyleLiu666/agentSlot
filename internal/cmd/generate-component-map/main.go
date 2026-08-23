package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LyleLiu666/agentSlot/componentcatalog"
)

func main() {
	root := flag.String("root", ".", "AgentSlot repository root")
	flag.Parse()
	files := []struct {
		name   string
		locale componentcatalog.Locale
	}{
		{name: "COMPONENT_MAP.md", locale: componentcatalog.LocaleEnglish},
		{name: "COMPONENT_MAP.zh-CN.md", locale: componentcatalog.LocaleChinese},
	}
	for _, file := range files {
		path := filepath.Join(*root, file.name)
		current, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		generated, err := componentcatalog.RewriteMarkdown(file.locale, current)
		if err != nil {
			fatal(err)
		}
		if bytes.Equal(current, generated) {
			continue
		}
		if err := os.WriteFile(path, generated, 0o644); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
