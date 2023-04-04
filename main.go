package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Ports []Port `toml:"port"`
}

type Port struct {
	Service  string   `toml:"service"`
	Mappings []string `toml:"mappings"`
}

func main() {
	var config Config
	_, errReadConfig := toml.DecodeFile("kubedev.toml", &config)

	if errReadConfig != nil {
		panic("reading config kubedev.toml: " + errReadConfig.Error())
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		time.Sleep(10 * time.Second)
		log.Println("shutting down forcefully")
		os.Exit(1)
	}()

	group := taskGroup{
		ctx:          ctx,
		maxRestarts:  3,
		restartSleep: 5 * time.Second,
		fatalError: func(err error) bool {
			return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		},
	}

	for _, port := range config.Ports {
		port := port
		group.Run("port-forward-"+port.Service, func() error {
			return portForward(ctx, port)
		})
	}

	errWait := group.Wait()
	switch {
	case errors.Is(errWait, context.Canceled):
		log.Println("shutting down")
	case errWait != nil:
		panic(errWait)
	}
}

type taskGroup struct {
	ctx          context.Context
	maxRestarts  int
	restartSleep time.Duration
	fatalError   func(error) bool

	wg    sync.WaitGroup
	errMu sync.Mutex
	errs  []error
}

func (tg *taskGroup) Wait() error {
	tg.wg.Wait()

	tg.errMu.Lock()
	defer tg.errMu.Unlock()
	return errors.Join(tg.errs...)
}

func (tg *taskGroup) Run(name string, task func() error) {
	ctx := tg.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	maxRestarts := tg.maxRestarts
	fatalError := func(error) bool { return false }
	if tg.fatalError != nil {
		fatalError = tg.fatalError
	}

	tg.wg.Add(1)

	go func() {
		defer tg.wg.Done()
		var err error
		restarts := 0
		for restarts := 0; restarts <= maxRestarts; restarts++ {
			if restarts > 0 {
				log.Printf(" %s is restarting %d/%d", name, restarts, maxRestarts)
			}
			err = task()
			log.Printf("%s exited with error: %v", name, err)
			if fatalError(err) {
				log.Printf("%s is fatal, exiting", name)
				break
			}
			log.Printf("%s will restart in %v", name, tg.restartSleep)

			if errSleep := sleep(ctx, tg.restartSleep); errSleep != nil {
				err = fmt.Errorf("sleep: %w", errSleep)
			}
		}

		if err != nil {
			err = fmt.Errorf("%s: %d restarts: %w", name, restarts, err)
		}

		tg.errMu.Lock()
		defer tg.errMu.Unlock()
		tg.errs = append(tg.errs, err)
	}()
}

func logPrefix(prefix string) io.WriteCloser {
	re, wr := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(re)
		for scanner.Scan() {
			log.Printf("%s%s", prefix, scanner.Text())
		}
	}()

	return wr
}

func portForward(ctx context.Context, port Port) error {
	svcName := "svc/" + strings.TrimPrefix(port.Service, "svc/")

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward", svcName)
	cmd.Args = append(cmd.Args, port.Mappings...)
	cmd.Env = append(cmd.Env, os.Environ()...)

	stderr := logPrefix("port-forwar " + svcName + ":stderr ")
	defer stderr.Close()
	cmd.Stderr = stderr

	stdout := logPrefix("port-forward " + svcName + ":stdout ")
	defer stdout.Close()
	cmd.Stdout = stdout

	log.Printf("staring port-forward %s", cmd)
	return cmd.Run()
}

func sleep(ctx context.Context, dt time.Duration) error {
	timer := time.NewTimer(dt)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
