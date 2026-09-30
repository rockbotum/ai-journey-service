package config

import "os"

// Indirection over the os package so tests can isolate the environment without
// mutating process state through a global.

func lookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

func setEnvRaw(key, value string) error { return os.Setenv(key, value) }

func unsetEnvRaw(key string) error { return os.Unsetenv(key) }
