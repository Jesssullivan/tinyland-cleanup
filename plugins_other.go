//go:build !darwin

package main

import "github.com/Jesssullivan/tinyland-cleanup/plugins"

func registerDarwinPlugins(registry *plugins.Registry) {
	// Darwin-specific plugins are not available on other platforms
	_ = registry
}

func registerLinuxPlugins(registry *plugins.Registry) {
	registry.Register(plugins.NewGitHubRunnerPlugin())
	registry.Register(plugins.NewYumPlugin())
}
