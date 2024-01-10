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

<!--
From Example_incorrectUsage:

The point here is: once an unhandled panic occurs the program is in a
unreasonable state and since that state cannot be reasoned about you
*should* immediately begin termination. Put differently, if you had
the foresight to understand the state that triggered the panic you
would have prevented that panic from occurring in the first place.
-->

## Installation

panics is available using the standard `go get` command.

Install by running:

    go get github.com/charlievieth/panics

<!--
## Usage / Notes

This package is designed to gracefully handle ill-behaved programs but itself
must be used properly. That is, you may not close notified channels, pass nil
Contexts, or WaitGroups since these represent very hard programming errors and
allowing them would essentially defeat the purpose of this package.

### First

[First](https://pkg.go.dev/github.com/charlievieth/panics#First) will return
the first capt

## Immediate panic writing

Exists as a backup and cannot be relied on because of slow writers / loggers
 -->
