package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lowne/supercronic/cron"
	"github.com/lowne/supercronic/crontab"
	"github.com/lowne/supercronic/log/hook"
	"github.com/lowne/supercronic/prometheus_metrics"
	"github.com/evalphobia/logrus_sentry"
	"github.com/sirupsen/logrus"
)

var Usage = func() {
	fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] CRONTAB\n\nAvailable options:\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	debug := flag.Bool("debug", false, "enable debug logging")
	json := flag.Bool("json", false, "enable JSON logging")
	test := flag.Bool("test", false, "test crontab (does not run jobs)")
	prometheusListen := flag.String("prometheus-listen-address", "", "give a valid ip:port address to expose Prometheus metrics at /metrics")
	splitLogs := flag.Bool("split-logs", false, "split log output into stdout/stderr")
	sentry := flag.String("sentry-dsn", "", "enable Sentry error logging, using provided DSN")
	sentryAlias := flag.String("sentryDsn", "", "alias for sentry-dsn")
	overlapping := flag.Bool("overlapping", false, "enable tasks overlapping")
	delay := flag.Int("delay", 0, "delay in seconds after executing @start")
	flag.Parse()

	var sentryDsn string

	if *sentryAlias != "" {
		sentryDsn = *sentryAlias
	}

	if *sentry != "" {
		sentryDsn = *sentry
	}

	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if *json {
		logrus.SetFormatter(&logrus.JSONFormatter{})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	}
	if *splitLogs {
		hook.RegisterSplitLogger(
			logrus.StandardLogger(),
			os.Stdout,
			os.Stderr,
		)
	}

	if flag.NArg() != 1 {
		Usage()
		os.Exit(2)
		return
	}

	crontabFileName := flag.Args()[0]

	var sentryHook *logrus_sentry.SentryHook
	if sentryDsn != "" {
		sentryLevels := []logrus.Level{
			logrus.PanicLevel,
			logrus.FatalLevel,
			logrus.ErrorLevel,
		}
		sh, err := logrus_sentry.NewSentryHook(sentryDsn, sentryLevels)
		if err != nil {
			logrus.Fatalf("Could not init sentry logger: %s", err)
		} else {
			sh.Timeout = 5 * time.Second
			sentryHook = sh
		}

		if sentryHook != nil {
			logrus.StandardLogger().AddHook(sentryHook)
		}
	}

	promMetrics := prometheus_metrics.NewPrometheusMetrics()

	if *prometheusListen != "" {
		promServerShutdownClosure, err := prometheus_metrics.InitHTTPServer(*prometheusListen, context.Background())
		if err != nil {
			logrus.Fatalf("prometheus http startup failed: %s", err.Error())
		}

		defer func() {
			if err := promServerShutdownClosure(); err != nil {
				logrus.Fatalf("prometheus http shutdown failed: %s", err.Error())
			}
		}()
	}

	for true {
		promMetrics.Reset()

		logrus.Infof("read crontab: %s", crontabFileName)
		tab, err := readCrontabAtPath(crontabFileName)

		if err != nil {
			logrus.Fatal(err)
			break
		}

		if *test {
			logrus.Info("crontab is valid")
			os.Exit(0)
			break
		}

		var wg sync.WaitGroup
		exitCtx, notifyExit := context.WithCancel(context.Background())

		for _, job := range tab.Jobs {
			cronLogger := logrus.WithFields(logrus.Fields{
				"job.schedule": job.Schedule,
				"job.command":  job.Command,
				"job.position": job.Position,
			})
			if job.Position != 0 && job.Schedule == crontab.Start {
				logrus.Fatalf(crontab.StartErr)
			}
			cron.StartJob(&wg, tab.Context, job, exitCtx, cronLogger, *overlapping, &promMetrics)
			if job.Schedule == crontab.Start {
				time.Sleep(time.Duration(*delay) * time.Second)
			}
		}

		termChan := make(chan os.Signal, 1)
		signal.Notify(termChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR2)

		termSig := <-termChan

		if termSig == syscall.SIGUSR2 {
			logrus.Infof("received %s, reloading crontab", termSig)
		} else {
			logrus.Infof("received %s, shutting down", termSig)
		}
		notifyExit()

		logrus.Info("waiting for jobs to finish")
		wg.Wait()

		if termSig != syscall.SIGUSR2 {
			logrus.Info("exiting")
			break
		}
	}
}

func readCrontabAtPath(path string) (*crontab.Crontab, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer file.Close()

	return crontab.ParseCrontab(file)
}
