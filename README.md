[![Tests](https://github.com/charlievieth/panics/actions/workflows/test.yml/badge.svg)](https://github.com/charlievieth/panics/actions/workflows/test.yml)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](https://pkg.go.dev/github.com/charlievieth/panics)

# panics

Package panics allows for panics to be safely handled and notified on.
It provides helper functions that can safely handle and recover from
unhandled panics and an API similar to [os/signal](https://pkg.go.dev/os/signal)
for panic notifications.

The goal of this package is to provide programs a way to centralize panic
handling and thus coordinate an orderly shutdown once a panic is detected.
It also allows for programs to control how panics and their associated stack
trace are logged.

This package does not make unhandled panics safe (nothing can), but is instead
designed to prevent an unhandled panic from abruptly halting a program before
any cleanup, termination, or alerting can be completed.

When a unhandled panic is detected the program should immediately initiate
shutdown. A panic indicates that the program is in an unreasonable state and
continuing execution may result in undefined behavior.

## Installation

panics is available using the standard `go get` command.

Install by running:

    go get github.com/charlievieth/panics

<!--
## Usage / Notes

### First

[First](https://pkg.go.dev/github.com/charlievieth/panics#First) will return
the first capt
 -->
