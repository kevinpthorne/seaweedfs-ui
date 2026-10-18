package web

import "embed"

// Content holds all embedded frontend single-page application assets.
//
//go:embed index.html css/* js/*
var Content embed.FS
