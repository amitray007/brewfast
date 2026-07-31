// Command brewfast accelerates Homebrew cask installs whose downloads come from
// throttled GitHub release assets: it fetches the asset with aria2's parallel
// connections, verifies the checksum against the cask definition, places the
// file in brew's download cache, then hands off to brew for the real install.
//
// This is a thin entry point; all command wiring lives in package cmd.
package main

import "github.com/amitray007/brewfast/cmd"

func main() {
	cmd.Execute()
}
