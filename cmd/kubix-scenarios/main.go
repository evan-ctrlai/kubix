// kubix-scenarios is an opt-in, read-only laboratory validation entry point.
package main

import (
	"flag"
	"fmt"
	"kubix/internal/labtest"
	"os"
)

func main() {
	var o labtest.ScenarioOptions
	flag.StringVar(&o.Root, "root", ".", "repository root")
	flag.StringVar(&o.Scenario, "scenario", "", "stale-current, healthy-history or service-mismatch")
	flag.StringVar(&o.Prefix, "prefix", "", "unique fixture prefix from operator script")
	flag.StringVar(&o.ModelURL, "model-url", "", "optional loopback local-model /v1 endpoint")
	flag.StringVar(&o.Model, "model", "", "local served model name")
	flag.BoolVar(&o.AllowLive, "allow-live", false, "explicitly permit read-only isolated-lab validation")
	flag.Parse()
	r, err := labtest.RunScenario(o)
	path, saveErr := labtest.SaveScenarioReport(o.Root, o.Prefix, r)
	if saveErr != nil {
		fmt.Fprintln(os.Stderr, "Report not saved:", saveErr)
		os.Exit(1)
	}
	fmt.Println("Report:", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("PASS %s: %d ms, %d captured sources, %d model requests\n", r.Scenario, r.InvestigationMS, r.CapturedSources, r.ModelRequests)
}
