// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

package checks

import (
	"strings"
	"time"

	"github.com/DataDog/datadog-agent/pkg/util/cache"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/gopsutil/process"
)

type processes map[int32]*process.FilledProcess

const (
	processCacheKey string = "compliance-processes"
)

type processFetcherFunc func() (map[int32]*process.FilledProcess, error)

var (
	processFetcher processFetcherFunc = process.AllProcesses
)

func (p processes) findProcessesByName(name string) []*process.FilledProcess {
	return p.findProcesses(func(process *process.FilledProcess) bool {
		return process.Name == name
	})
}

func (p processes) findProcesses(matchFunc func(*process.FilledProcess) bool) []*process.FilledProcess {
	var results = make([]*process.FilledProcess, 0)
	for _, process := range p {
		if matchFunc(process) {
			results = append(results, process)
		}
	}

	return results
}

func getProcesses(maxAge time.Duration) (processes, error) {
	if value, found := cache.Cache.Get(processCacheKey); found {
		return value.(processes), nil
	}

	log.Debug("Updating process cache")
	cachedProcesses, err := processFetcher()
	if err != nil {
		return nil, err
	}

	cache.Cache.Set(processCacheKey, cachedProcesses, maxAge)
	return cachedProcesses, nil
}

// Parsing is far from being exhaustive, however for now it works sufficiently well
// for standard flag style command args.
func parseProcessCmdLine(args []string) map[string]string {
	results := make(map[string]string, 0)
	pendingFlagValue := false

	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			parts := strings.SplitN(arg, "=", 2)

			// We have -xxx=yyy, considering the flag completely resolved
			if len(parts) == 2 {
				results[parts[0]] = parts[1]
			} else {
				results[parts[0]] = ""
				pendingFlagValue = true
			}
		} else {
			if pendingFlagValue {
				results[args[i-1]] = arg
			} else {
				results[arg] = ""
			}
		}
	}

	return results
}
