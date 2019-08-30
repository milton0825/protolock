package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/nilslice/protolock"
	"github.com/nilslice/protolock/extend"
)

func runPlugins(pluginList string, report *protolock.Report) (*protolock.Report, error) {
	inputData := &bytes.Buffer{}

	err := json.NewEncoder(inputData).Encode(&extend.Data{
		Current:           report.Current,
		Updated:           report.Updated,
		ProtolockWarnings: report.Warnings,
		PluginWarnings:    []protolock.Warning{},
	})
	if err != nil {
		return nil, err
	}

	// collect plugin warnings and errors as they are returned from plugins
	pluginWarningsChan := make(chan []protolock.Warning)
	pluginsDone := make(chan struct{})
	pluginErrsChan := make(chan error)
	var allPluginErrors []error
	go func() {
		for {
			select {
			case <-pluginsDone:
				return

			case err := <-pluginErrsChan:
				if err != nil {
					allPluginErrors = append(allPluginErrors, err)
				}

			case warnings := <-pluginWarningsChan:
				for _, warning := range warnings {
					report.Warnings = append(report.Warnings, warning)
				}
			}
		}
	}()

	wg := &sync.WaitGroup{}
	plugins := strings.Split(pluginList, ",")
	for _, name := range plugins {
		wg.Add(1)

		// copy input data to be passed in to and processed by each plugin
		pluginInputData := bytes.NewReader(inputData.Bytes())

		// run all provided plugins in parallel, each recieving the same current
		// and updated Protolock structs from the `protolock status` call
		go func(name string) {
			defer wg.Done()
			name = strings.TrimSpace(name)
			path, err := exec.LookPath(name)
			if err != nil {
				if path == "" {
					path = name
				}
				fmt.Println("[protolock] plugin exec error:", err)
				return
			}

			// initialize the executable to be called from protolock using the
			// absolute path and copy of the input data
			plugin := &exec.Cmd{
				Path:  path,
				Stdin: pluginInputData,
			}

			// execute the plugin and capture the output
			output, err := plugin.CombinedOutput()
			if err != nil {
				pluginErrsChan <- wrapPluginErr(name, path, err, output)
				return
			}

			pluginData := &extend.Data{}
			err = json.Unmarshal(output, pluginData)
			if err != nil {
				fmt.Println("[protolock] Get following message:", string(output))
				fmt.Println("[protolock] plugin data decode error:", err)
				return
			}

			// gather all warnings from each plugin, and send to warning chan
			// collector as a slice to keep together
			if pluginData.PluginWarnings != nil {
				pluginWarningsChan <- pluginData.PluginWarnings
			}

			if pluginData.PluginErrorMessage != "" {
				pluginErrsChan <- wrapPluginErr(
					name, path, errors.New(pluginData.PluginErrorMessage), output,
				)
			}
		}(name)
	}

	wg.Wait()
	pluginsDone <- struct{}{}

	if allPluginErrors != nil {
		var errorMsgs []string
		for _, pluginError := range allPluginErrors {
			errorMsgs = append(errorMsgs, pluginError.Error())
		}

		return nil, fmt.Errorf(
			`[protolock:plugin] accumulated plugin errors:
%s`,
			strings.Join(errorMsgs, "\n"),
		)
	}

	return report, nil
}

func wrapPluginErr(name, path string, err error, output []byte) error {
	out := strings.ReplaceAll(
		string(output), protolock.ProtoSep, protolock.FileSep,
	)
	return fmt.Errorf("%s (%s): %v\n%s", name, path, err, out)
}
