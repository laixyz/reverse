// Copyright 2019 The Xorm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cmd

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/laixyz/reverse/language"

	"gitea.com/lunny/log"
	"github.com/gobwas/glob"
	xorm "github.com/laixyz/xormplus"
	"github.com/laixyz/xormplus/names"
	"github.com/laixyz/xormplus/schemas"
	"gopkg.in/yaml.v2"
)

func reverse(rFile string) error {
	f, err := os.Open(rFile)
	if err != nil {
		return err
	}
	defer f.Close()
	return reverseFromReader(f)
}

func reverseFromReader(rd io.Reader) error {
	var cfg ReverseConfig
	err := yaml.NewDecoder(rd).Decode(&cfg)
	if err != nil {
		return err
	}
	for _, target := range cfg.Targets {
		if err := runReverse(&cfg.Source, &target); err != nil {
			return err
		}
	}

	return nil
}

// ReverseSource represents a reverse source which should be a database connection
type ReverseSource struct {
	Database string `yaml:"database"`
	ConnStr  string `yaml:"conn_str"`
}

// ReverseTarget represents a reverse target
type ReverseTarget struct {
	Type              string   `yaml:"type"`
	IncludeTables     []string `yaml:"include_tables"`
	ExcludeTables     []string `yaml:"exclude_tables"`
	TableMapper       string   `yaml:"table_mapper"`
	ColumnMapper      string   `yaml:"column_mapper"`
	TemplatePath      string   `yaml:"template_path"`
	ModelTemplatePath string   `yaml:"model_template_path"`
	Template          string   `yaml:"template"`
	MultipleFiles     bool     `yaml:"multiple_files"`
	OutputDir         string   `yaml:"output_dir"`
	TablePrefix       string   `yaml:"table_prefix"`
	Language          string   `yaml:"language"`
	TableName         bool     `yaml:"table_name"`

	Funcs     map[string]string `yaml:"funcs"`
	Formatter string            `yaml:"formatter"`
	Importter string            `yaml:"importter"`
	ExtName   string            `yaml:"ext_name"`
}

// ReverseConfig represents a reverse configuration
type ReverseConfig struct {
	Kind    string          `yaml:"kind"`
	Name    string          `yaml:"name"`
	Source  ReverseSource   `yaml:"source"`
	Targets []ReverseTarget `yaml:"targets"`
}

var (
	formatters   = map[string]func(string) (string, error){}
	importters   = map[string]func([]*schemas.Table) []string{}
	defaultFuncs = template.FuncMap{
		"UnTitle": unTitle,
		"Upper":   upTitle,
	}
)

func unTitle(src string) string {
	if src == "" {
		return ""
	}

	if len(src) == 1 {
		return strings.ToLower(string(src[0]))
	}
	return strings.ToLower(string(src[0])) + src[1:]
}

func upTitle(src string) string {
	if src == "" {
		return ""
	}

	return strings.ToUpper(src)
}

func filterTables(tables []*schemas.Table, target *ReverseTarget) []*schemas.Table {
	var res = make([]*schemas.Table, 0, len(tables))
	for _, tb := range tables {
		var remove bool
		for _, exclude := range target.ExcludeTables {
			s, _ := glob.Compile(exclude)
			remove = s.Match(tb.Name)
			if remove {
				break
			}
		}
		if remove {
			continue
		}
		if len(target.IncludeTables) == 0 {
			res = append(res, tb)
			continue
		}

		var keep bool
		for _, include := range target.IncludeTables {
			s, _ := glob.Compile(include)
			keep = s.Match(tb.Name)
			if keep {
				break
			}
		}
		if keep {
			res = append(res, tb)
		}
	}
	return res
}

func newFuncs() template.FuncMap {
	var m = make(template.FuncMap)
	for k, v := range defaultFuncs {
		m[k] = v
	}
	return m
}

func convertMapper(mapname string) names.Mapper {
	switch mapname {
	case "gonic":
		return names.LintGonicMapper
	case "same":
		return names.SameMapper{}
	default:
		return names.SnakeMapper{}
	}
}

func runReverse(source *ReverseSource, target *ReverseTarget) error {
	orm, err := xorm.NewEngine(source.Database, source.ConnStr)
	if err != nil {
		return err
	}

	tables, err := orm.DBMetas()
	if err != nil {
		return err
	}

	// filter tables according includes and excludes
	tables = filterTables(tables, target)

	// load configuration from language
	lang := language.GetLanguage(target.Language, target.TableName)
	funcs := newFuncs()
	formatter := formatters[target.Formatter]
	importter := importters[target.Importter]

	// load template
	var bs []byte
	if target.Template != "" {
		bs = []byte(target.Template)
	} else if target.TemplatePath != "" {
		bs, err = ioutil.ReadFile(target.TemplatePath)
		if err != nil {
			return err
		}
	}
	var ModelName = "model"
	ModelName, err = GetPackageName(target.OutputDir)
	if err != nil {
		ModelName = "model"
	}
	if lang != nil {
		if bs == nil {

			bs = []byte(lang.Template)
		}
		for k, v := range lang.Funcs {
			funcs[k] = v
		}
		if formatter == nil {
			formatter = lang.Formatter
		}
		if importter == nil {
			importter = lang.Importter
		}
		target.ExtName = lang.ExtName
	}
	if !strings.HasPrefix(target.ExtName, ".") {
		target.ExtName = "." + target.ExtName
	}

	var tableMapper = convertMapper(target.TableMapper)
	var colMapper = convertMapper(target.ColumnMapper)

	funcs["TableMapper"] = tableMapper.Table2Obj
	funcs["ColumnMapper"] = colMapper.Table2Obj

	if bs == nil {
		return errors.New("You have to indicate template / template path or a language")
	}

	t := template.New("reverse")
	t.Funcs(funcs)

	tmpl, err := t.Parse(string(bs))
	if err != nil {
		return err
	}

	for _, table := range tables {
		if target.TablePrefix != "" {
			table.Name = strings.TrimPrefix(table.Name, target.TablePrefix)
		}
		for _, col := range table.Columns() {
			col.FieldName = colMapper.Table2Obj(col.Name)
		}
	}

	err = os.MkdirAll(target.OutputDir, os.ModePerm)
	if err != nil {
		return err
	}

	var w *os.File
	if !target.MultipleFiles {
		w, err = os.Create(filepath.Join(target.OutputDir, "models"+target.ExtName))
		if err != nil {
			return err
		}
		defer w.Close()

		imports := importter(tables)

		newbytes := bytes.NewBufferString("")
		err = tmpl.Execute(newbytes, map[string]interface{}{
			"Tables":    tables,
			"Imports":   imports,
			"ModelName": ModelName,
		})
		if err != nil {
			return err
		}

		tplcontent, err := ioutil.ReadAll(newbytes)
		if err != nil {
			return err
		}
		var source string
		if formatter != nil {
			source, err = formatter(string(tplcontent))
			if err != nil {
				log.Warnf("%v", err)
				source = string(tplcontent)
			}
		} else {
			source = string(tplcontent)
		}

		w.WriteString(source)
		w.Close()
	} else {
		for _, table := range tables {
			// imports
			tbs := []*schemas.Table{table}
			imports := importter(tbs)

			w, err := os.Create(filepath.Join(target.OutputDir, table.Name+target.ExtName))
			if err != nil {
				return err
			}
			defer w.Close()

			newbytes := bytes.NewBufferString("")
			err = tmpl.Execute(newbytes, map[string]interface{}{
				"Tables":    tbs,
				"Imports":   imports,
				"ModelName": ModelName,
			})
			if err != nil {
				return err
			}

			tplcontent, err := ioutil.ReadAll(newbytes)
			if err != nil {
				return err
			}
			var source string
			if formatter != nil {
				source, err = formatter(string(tplcontent))
				if err != nil {
					log.Warnf("%v", err)
					source = string(tplcontent)
				}
			} else {
				source = string(tplcontent)
			}

			w.WriteString(source)
			w.Close()
		}

	}
	if target.ModelTemplatePath != "" {
		return ModelCodeFileCreate(target.ModelTemplatePath, target.OutputDir)
	}

	return nil
}

// GetPackageName 通过实际路径，得到包名
func GetPackageName(Path string) (string, error) {
	path, err := filepath.Abs(Path)
	if err == nil {
		var fileInfo os.FileInfo
		fileInfo, err = os.Stat(path)
		if err == nil {
			return fileInfo.Name(), nil
		}
	}
	return "", err
}

//
func ModelCodeFileCreate(templatePath, targePath string) error {
	ModelName, err := GetPackageName(targePath)
	if err != nil {
		return err
	}
	bs, err := ioutil.ReadFile(templatePath)
	if err != nil {
		return err
	}

	t := template.New("model_base")

	tmpl, err := t.Parse(string(bs))
	if err != nil {
		return err
	}

	w, err := os.Create(filepath.Join(targePath, "model_base.go"))
	if err != nil {
		return err
	}
	defer w.Close()

	newbytes := bytes.NewBufferString("")
	err = tmpl.Execute(newbytes, map[string]interface{}{
		"ModelName": ModelName,
	})
	if err != nil {
		return err
	}

	tplcontent, err := ioutil.ReadAll(newbytes)
	if err != nil {
		return err
	}
	var source string

	source = string(tplcontent)

	w.WriteString(source)
	w.Close()
	return nil
}
