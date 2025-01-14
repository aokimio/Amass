// Copyright 2017-2021 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package config

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/aokimio/Amass/v3/resources"
)

// AcquireScripts returns all the default and user provided scripts for data sources.
func (c *Config) AcquireScripts() ([]string, error) {
	scripts, err := resources.GetDefaultScripts()
	if err != nil {
		return scripts, err
	}

	dir := OutputDirectory(c.Dir)
	if dir == "" {
		return scripts, nil
	}

	finfo, err := os.Stat(dir)
	if os.IsNotExist(err) || !finfo.IsDir() {
		return scripts, errors.New("the output directory does not exist or is not a directory")
	}

	paths := []string{filepath.Join(dir, "scripts")}
	if c.ScriptsDirectory != "" {
		paths = append(paths, c.ScriptsDirectory)
	}

	for _, path := range paths {
		_ = filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// Is this file not a script?
			if info.IsDir() || filepath.Ext(info.Name()) != ".ads" {
				return nil
			}
			// Get the script content
			data, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}

			scripts = append(scripts, string(data))
			return nil
		})
	}

	return scripts, nil
}
