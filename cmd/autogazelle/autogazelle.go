/* Copyright 2018 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// autogazelle is a program that tracks changes in a workspace and runs
// gazelle to incorporate those changes into Bazel build files.
//
// autogazelle has two components: a client and a server. The server
// watches for file system changes within the workspace and builds a
// set of build files that need to be updated. The server listens on a
// UNIX socket. When it accepts a connection, it runs gazelle in modified
// directories and closes the connection without transmitting anything.
// The client simply connects to the server and waits for the connection
// to be closed.
//
// autogazelle is intended to be invoked by autogazelle.bash as a bazel
// wrapper script. It requires the BUILD_WORKSPACE_DIRECTORY environment
// variable to be set to the workspace root directory and BAZEL_REAL to
// be set to the local of the real bazel binary.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/bazel-gazelle/walk"
)

var (
	programName = filepath.Base(os.Args[0])

	isServer      = flag.Bool("server", false, "whether this process acts as the server")
	gazelleLabel  = flag.String("gazelle", "", "label for script that autogazelle should invoke with 'bazel run'")
	serverTimeout = flag.Duration("timeout", 3600*time.Second, "time in seconds the server will listen for a client before quitting")
	socketPath    = flag.String("socket", "tools/autogazelle.socket", "path to the UNIX socket where the server will listen, relative to the workspace root")
	logPath       = flag.String("log", "tools/autogazelle.log", "path to the server's log file, relative to the workspace root")
)

func main() {
	log.SetPrefix(programName + ": ")
	log.SetFlags(log.Ldate | log.Ltime)
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if *gazelleLabel == "" {
		return errors.New("-gazelle not set")
	}

	workspaceDir, ok := os.LookupEnv("BUILD_WORKSPACE_DIRECTORY")
	if !ok {
		return errors.New("BUILD_WORKSPACE_DIRECTORY not set")
	}
	if err := os.Chdir(workspaceDir); err != nil {
		return err
	}

	if _, ok := os.LookupEnv("BAZEL_REAL"); !ok {
		return errors.New("BAZEL_REAL not set")
	}

	if *isServer {
		return runServer()
	} else {
		return runClient()
	}
}

type mode int

const (
	fullMode mode = iota
	fastMode
)

// runGazelle invokes gazelle with "bazel run". In fullMode, gazelle will
// run in the entire repository. In fastMode, gazelle will only run
// in the given directories.
func runGazelle(mode mode, dirs []string) error {
	if mode == fastMode && len(dirs) == 0 {
		return nil
	}

	args := []string{os.Getenv("BAZEL_REAL"), "run", *gazelleLabel, "--", "-args"}
	args = append(args, "-index=false")
	if mode == fastMode {
		args = append(args, "-r=false")
		args = append(args, dirs...)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	log.Printf("running gazelle: %s\n", strings.Join(cmd.Args, " "))
	return cmd.Run()
}

// restoreBuildFilesInRepo copies BUILD.in and BUILD.bazel.in files and
// copies them to BUILD and BUILD.bazel.
func restoreBuildFilesInRepo() {
	walkWorkspace(".", func(dir string, files []string) {
		for _, file := range files {
			if file != "BUILD.bazel.in" && file != "BUILD.in" {
				continue
			}

			path := filepath.Join(dir, file)
			if err := restoreFile(path, strings.TrimSuffix(path, ".in")); err != nil {
				log.Print(err)
			}
		}
	})
}

func restoreBuildFilesInDir(dir string) {
	for _, base := range []string{"BUILD.bazel", "BUILD"} {
		inPath := filepath.Join(dir, base+".in")
		_, err := os.Stat(inPath)
		if err != nil {
			continue
		}
		if err := restoreFile(inPath, filepath.Join(dir, base)); err != nil {
			log.Print(err)
		}
	}
}

func restoreFile(src, dest string) (err error) {
	r, err := os.Open(src)
	if err != nil {
		return err
	}
	defer r.Close()

	w, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := w.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	_, err = fmt.Fprintf(w, `# This file was generated from %s
# by %s
# DO NOT EDIT

`, filepath.Base(src), programName)
	if err != nil {
		return err
	}

	_, err = io.Copy(w, r)
	return err
}

// walkWorkspace traverses the directory tree rooted at `root`, while respecting
// gazelle directives including exclude, follow, and ignore. `walkFunc` will be
// called for each directory with `dir` being the absolute path to the directory,
// and `files` is a list of base names of regular files within the directory, not
// including excluded files.
func walkWorkspace(root string, walkFunc func(dir string, files []string)) {
	cext := &walk.Configurer{}
	cexts := []config.Configurer{cext}
	for _, lang := range languages {
		cexts = append(cexts, lang)
	}

	root, err := filepath.Abs(root)
	if err != nil {
		log.Printf("failed to find absolute path: %v", err)
		return
	}

	c := config.New()
	c.RepoRoot = root
	for _, cext := range cexts {
		cext.RegisterFlags(&flag.FlagSet{}, "", c)
	}

	dirs := []string{root}

	mode := walk.VisitAllUpdateSubdirsMode

	walk.Walk(c, cexts, dirs, mode, func(dir, rel string, c *config.Config, update bool, f *rule.File, subdirs, regularFiles, genFiles []string) {
		walkFunc(dir, regularFiles)
	})
}
