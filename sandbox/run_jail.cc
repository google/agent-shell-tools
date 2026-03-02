// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
   run_jail — nsjail execution backend for agent-sandbox

   Provides a C-linkage entry point that accepts a fully-constructed
   protobuf-encoded NsJailConfig and runs the jail.  The Rust frontend
   owns config construction; this file owns nsjail plumbing.
*/

#include "run_jail.h"

#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/time.h>
#include <termios.h>
#include <unistd.h>

#include <atomic>
#include <memory>
#include <string>

#include "cgroup2.h"
#include "cmdline.h"
#include "config.h"
#include "logs.h"
#include "macros.h"
#include "sandbox.h"
#include "subproc.h"
#include "user.h"
#include "util.h"

// ---------------------------------------------------------------------------
// Glue copied from nsjail.cc (these are static there, so we reproduce them).
// ---------------------------------------------------------------------------

static std::atomic<int> sigFatal(0);
static std::atomic<bool> showProc(false);

static void sigHandler(int sig) {
	if (sig == SIGALRM || sig == SIGCHLD || sig == SIGPIPE) {
		return;
	}
	if (sig == SIGUSR1 || sig == SIGQUIT) {
		showProc = true;
		return;
	}
	sigFatal = sig;
}

static bool setSigHandlers() {
	for (const auto sig : nssigs) {
		sigset_t smask;
		sigemptyset(&smask);

		struct sigaction sa;
		sa.sa_handler = sigHandler;
		sa.sa_mask = smask;
		sa.sa_flags = 0;
		sa.sa_restorer = NULL;

		if (sig == SIGTTIN || sig == SIGTTOU) {
			sa.sa_handler = SIG_IGN;
		}
		if (sigaction(sig, &sa, NULL) == -1) {
			PLOG_E("sigaction(%d)", sig);
			return false;
		}
	}
	return true;
}

static bool setTimer(nsj_t* nsj) {
	if (nsj->njc.mode() == nsjail::Mode::EXECVE) {
		return true;
	}
	struct itimerval it = {
	    .it_interval = {.tv_sec = 1, .tv_usec = 0},
	    .it_value = {.tv_sec = 1, .tv_usec = 0},
	};
	if (setitimer(ITIMER_REAL, &it, NULL) == -1) {
		PLOG_E("setitimer(ITIMER_REAL)");
		return false;
	}
	return true;
}

static int standaloneMode(nsj_t* nsj) {
	for (;;) {
		if (subproc::runChild(nsj, -1, STDIN_FILENO, STDOUT_FILENO,
			STDERR_FILENO) == -1) {
			LOG_E("Couldn't launch the child process");
			return 0xff;
		}
		for (;;) {
			int child_status = subproc::reapProc(nsj);
			if (subproc::countProc(nsj) == 0) {
				if (nsj->njc.mode() == nsjail::Mode::ONCE) {
					return child_status;
				}
				break;
			}
			if (showProc) {
				showProc = false;
				subproc::displayProc(nsj);
			}
			if (sigFatal > 0) {
				subproc::killAndReapAll(nsj,
				    nsj->njc.forward_signals() ? sigFatal.load()
				                               : SIGKILL);
				logs::logStop(sigFatal);
				return (128 + sigFatal);
			}
			pause();
		}
	}
}

static std::unique_ptr<struct termios> getTC(int fd) {
	std::unique_ptr<struct termios> trm(new struct termios);
	if (ioctl(fd, TCGETS, trm.get()) == -1) {
		return nullptr;
	}
	return trm;
}

static void setTC(int fd, const struct termios* trm) {
	if (!trm) {
		return;
	}
	ioctl(fd, TCSETS, trm);
	tcflush(fd, TCIFLUSH);
}

// ---------------------------------------------------------------------------
// Config loading — replicates config::parseInternal (which is static).
// ---------------------------------------------------------------------------

static bool applyConfig(nsj_t* nsj, const nsjail::NsJailConfig& njc) {
	nsj->njc.CopyFrom(njc);

	if (njc.has_log_fd()) {
		logs::logFile("", njc.log_fd());
	}
	if (njc.has_log_file()) {
		logs::logFile(njc.log_file(), STDERR_FILENO);
	}
	if (njc.has_log_level()) {
		switch (njc.log_level()) {
		case nsjail::LogLevel::DEBUG:
			logs::setLogLevel(logs::DEBUG);
			break;
		case nsjail::LogLevel::INFO:
			logs::setLogLevel(logs::INFO);
			break;
		case nsjail::LogLevel::WARNING:
			logs::setLogLevel(logs::WARNING);
			break;
		case nsjail::LogLevel::ERROR:
			logs::setLogLevel(logs::ERROR);
			break;
		case nsjail::LogLevel::FATAL:
			logs::setLogLevel(logs::FATAL);
			break;
		default:
			LOG_E("Unknown log_level: %d", njc.log_level());
			return false;
		}
	}

	for (int i = 0; i < njc.envar_size(); i++) {
		cmdline::addEnv(nsj, njc.envar(i));
	}
	for (int i = 0; i < njc.pass_fd_size(); i++) {
		nsj->openfds.push_back(njc.pass_fd(i));
	}
	for (int i = 0; i < njc.uidmap_size(); i++) {
		if (!user::parseId(nsj, njc.uidmap(i).inside_id(),
			njc.uidmap(i).outside_id(), njc.uidmap(i).count(),
			false, njc.uidmap(i).use_newidmap())) {
			return false;
		}
	}
	for (int i = 0; i < njc.gidmap_size(); i++) {
		if (!user::parseId(nsj, njc.gidmap(i).inside_id(),
			njc.gidmap(i).outside_id(), njc.gidmap(i).count(),
			true, njc.gidmap(i).use_newidmap())) {
			return false;
		}
	}

	if (!njc.mount_proc()) {
		nsj->proc_path.clear();
	}
	if (njc.has_seccomp_policy_file()) {
		nsj->njc.set_seccomp_policy_file(njc.seccomp_policy_file());
	}

	if (njc.has_exec_bin()) {
		if (njc.exec_bin().has_path()) {
			nsj->argv.push_back(njc.exec_bin().path());
		}
		for (int i = 0; i < njc.exec_bin().arg().size(); i++) {
			nsj->argv.push_back(njc.exec_bin().arg(i));
		}
		if (njc.exec_bin().has_arg0()) {
			nsj->argv[0] = njc.exec_bin().arg0();
		}
		nsj->exec_fd = njc.exec_bin().exec_fd();
	}

	return true;
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

extern "C" int run_jail(const unsigned char* config_pb,
    size_t config_pb_len) {
	// Deserialize the final config from the Rust caller.
	nsjail::NsJailConfig njc;
	if (!njc.ParseFromArray(config_pb, static_cast<int>(config_pb_len))) {
		LOG_F("Failed to parse config protobuf");
	}

	// Initialize nsj_t and apply config.
	auto nsj = std::make_unique<nsj_t>();
	nsj->is_root_rw = false;
	nsj->is_proc_rw = false;
	nsj->proc_path = "/proc";
	nsj->orig_uid = getuid();
	nsj->orig_euid = geteuid();
	nsj->seccomp_fprog.filter = NULL;
	nsj->seccomp_fprog.len = 0;
	// Force legacy mount API — the new API (fsopen/fsconfig) misparses
	// tmpfs size= options as flags instead of key-value pairs.
	nsj->mnt_newapi = false;
	nsj->openfds.push_back(STDIN_FILENO);
	nsj->openfds.push_back(STDOUT_FILENO);
	nsj->openfds.push_back(STDERR_FILENO);

	if (!applyConfig(nsj.get(), njc)) {
		LOG_F("Failed to apply config");
	}

	// Fill in uid/gid if the config didn't provide mappings.
	if (nsj->uids.empty()) {
		idmap_t uid = {getuid(), getuid(), 1, false};
		nsj->uids.push_back(uid);
	}
	if (nsj->gids.empty()) {
		idmap_t gid = {getgid(), getgid(), 1, false};
		nsj->gids.push_back(gid);
	}

	// --- Standard nsjail startup sequence ---

	std::unique_ptr<struct termios> trm = getTC(STDIN_FILENO);

	cmdline::logParams(nsj.get());

	if (!setSigHandlers()) {
		LOG_F("setSigHandlers() failed");
	}
	if (!setTimer(nsj.get())) {
		LOG_F("setTimer() failed");
	}

	if (nsj->njc.detect_cgroupv2()) {
		cgroup2::detectCgroupv2(nsj.get());
	}
	if (nsj->njc.use_cgroupv2()) {
		if (!cgroup2::setup(nsj.get())) {
			LOG_E("Couldn't setup parent cgroup (cgroupv2)");
			return -1;
		}
	}

	if (!sandbox::preparePolicy(nsj.get())) {
		LOG_F("Couldn't prepare sandboxing policy");
	}

	int ret = standaloneMode(nsj.get());

	subproc::killAndReapAll(nsj.get(), SIGKILL);
	sandbox::closePolicy(nsj.get());
	setTC(STDIN_FILENO, trm.get());

	return ret;
}
