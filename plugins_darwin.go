//go:build darwin

package main

import "github.com/Jesssullivan/tinyland-cleanup/plugins"

func registerLinuxPlugins(registry *plugins.Registry) {
	// Linux-specific plugins are not available on Darwin
	_ = registry
}

func registerDarwinPlugins(registry *plugins.Registry) {
	registry.Register(plugins.NewHomebrewPlugin())
	registry.Register(plugins.NewIOSSimulatorPlugin())
	registry.Register(plugins.NewXcodePlugin())
	registry.Register(plugins.NewICloudPlugin())
	registry.Register(plugins.NewPhotosPlugin())
	registry.Register(plugins.NewLimaPlugin())
	registry.Register(plugins.NewAPFSPlugin())
}
