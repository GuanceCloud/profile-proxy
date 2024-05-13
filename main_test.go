package main

import (
	"fmt"
	"testing"
)

func TestResolveEndpoints(t *testing.T) {

	endpoints := []string{
		"127.0.0.1",
		"http://127.0.0.1",
		"https://127.0.0.1",
		"http://127.0.0.1/",
		"https://127.0.0.1/foo/bar/",
		"https://127.0.0.1/foo/bar/?hello=world",
		"https://127.0.0.1/foo/bar/?hello=world#help",
	}

	resolved, err := ResolveEndpoints(endpoints)
	if err != nil {
		t.Fatal(err)
	}

	for _, ep := range resolved {
		fmt.Printf("%s -> %s\n", ep.raw, ep.resolved.String())
	}

}
