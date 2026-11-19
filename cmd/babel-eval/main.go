package main

import (
	"os"

	"github.com/XavierJu007/Babel-Compiler/internal/evalrun"
)

func main() { os.Exit(evalrun.RunCLI(os.Args[1:], os.Stdout, os.Stderr)) }
