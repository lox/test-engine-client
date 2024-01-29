package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/buildkite/test-splitter/internal/api"
	"github.com/buildkite/test-splitter/internal/runner"
	"github.com/buildkite/test-splitter/internal/util"
)

// other attributes are omitted for simplicity
type Example struct {
	FilePath string `json:"file_path"`
}

// other attributes are omitted for simplicity
type RspecData struct {
	Examples []Example `json:"examples"`
}

func main() {
	// TODO: detect test runner and use appropriate runner
	testRunner := runner.Rspec{}

	// get files
	fmt.Println("--- :test-analytics: Gathering test plan context and creating test plan request 🐿️")
	files := testRunner.GetFiles()
	fmt.Printf("Found %d files\n", len(files))

	// fetch env vars
	suiteToken := util.FetchEnv("BUILDKITE_SPLITTER_RSPEC_TOKEN", "xx-local-analytics-key")
	identifier := util.FetchEnv("BUILDKITE_BUILD_ID", "local")
	mode := util.FetchEnv("BUILDKITE_SPLITTER_MODE", "static")
	parralelism := util.FetchIntEnv("BUILDKITE_PARALLEL_JOB_COUNT", 1)

	// get plan
	fmt.Println("--- :test-analytics: Getting Test Plan 🎣")
	fmt.Println("SuiteToken: ", suiteToken)
	fmt.Println("Mode: ", mode)
	fmt.Println("Identifier: ", identifier)
	fmt.Println("Parallelism: ", parralelism)

	testCases := []api.TestCase{}
	for _, file := range files {
		testCases = append(testCases, api.TestCase{
			Path: file,
		})
	}

	plan := api.GetTestPlan(api.TestPlanParams{
		SuiteToken:  suiteToken,
		Mode:        mode,
		Identifier:  identifier,
		Parallelism: parralelism,
		Tests: api.Tests{
			Cases:  testCases,
			Format: "files",
		},
	})

	// get plan for this node
	nodeIdx := util.FetchEnv("BUILDKITE_PARALLEL_JOB", "0")
	thisNodePlan := plan.Tasks[nodeIdx]

	prettifiedPlan, _ := json.MarshalIndent(thisNodePlan, "", "  ")
	fmt.Println("--- :test-analytics: Plan for this node 🎊")
	fmt.Println(string(prettifiedPlan))

	// execute tests
	runnableTests := []string{}
	for _, testCase := range thisNodePlan.Tests.Cases {
		runnableTests = append(runnableTests, testCase.Path)
	}
	err := testRunner.Run(runnableTests)

	if err != nil {
		// TODO: bubble up rspec error to main process
		log.Fatal("Error when executing tests: ", err)
	}

	fmt.Println("--- :test-analytics: Test execution results 📊")
	testRunner.Report(thisNodePlan.Tests.Cases)
}
