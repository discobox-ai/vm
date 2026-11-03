/*
 * krun-launch runs one libkrun VM (ADR 26-11-03-530 §2). The libkrun driver's
 * shim writes the VM's configuration to a file and execs this on it, through
 * the runtime's own dynamic loader, so libkrun, glibc, and every other library
 * come from the runtime and none from the host. It makes the libkrun calls and
 * ends in krun_start_enter, which returns only if the VM fails to start;
 * otherwise libkrun owns the process and exits it when the guest powers off.
 *
 *   krun-launch --version      prints the configuration version it reads
 *   krun-launch CONFIG         runs the VM CONFIG describes
 *
 * CONFIG is the launcher's and disco-vm's alone, versioned by its first line.
 * Each line is a keyword and its fields, separated by single spaces; a path is
 * always the last field, so it may hold spaces:
 *
 *   krun-launch 1
 *   cpus N
 *   memory MIB
 *   kernel PATH                  an uncompressed ELF vmlinux
 *   initrd PATH
 *   cmdline TEXT                 the whole kernel command line
 *   console PATH                 the guest's console, which libkrun truncates
 *   disk ID ro|rw PATH           a virtio-blk disk, in order: vda, vdb, ...
 *   vsock-in PORT PATH           a Unix socket libkrun listens on; a host
 *                                connection to it reaches guest port PORT
 *   vsock-out PORT PATH          a guest connection to the host's PORT
 *                                reaches the Unix socket PATH
 *   net FD MAC                   a virtio-net device on the connected
 *                                Unix stream FD, passt's
 *   share TAG ro|rw PATH         a virtiofs share of the host directory PATH
 *
 * The vsock device has no TSI: a guest reaches nothing of the host's but the
 * ports CONFIG maps, and the network device, if there is one.
 *
 * Every failure is a line on standard error and exit status 1, before the VM
 * starts; the driver returns them.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <libkrun.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define CONFIG_VERSION "1"

static const char *config_path;
static int line_no;

static void fail(const char *fmt, ...) __attribute__((noreturn, format(printf, 1, 2)));

static void fail(const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	fprintf(stderr, "krun-launch: ");
	if (line_no > 0)
		fprintf(stderr, "%s:%d: ", config_path, line_no);
	vfprintf(stderr, fmt, ap);
	fputc('\n', stderr);
	va_end(ap);
	exit(1);
}

/* check fails with what libkrun returned, a negative errno. */
static void check(int32_t ret, const char *call)
{
	if (ret < 0)
		fail("%s: %s", call, strerror(-ret));
}

/* field cuts the next space-separated field off *rest. */
static char *field(char **rest, const char *what)
{
	char *f = *rest;
	if (f == NULL || *f == '\0')
		fail("missing %s", what);
	char *sp = strchr(f, ' ');
	if (sp != NULL) {
		*sp = '\0';
		*rest = sp + 1;
	} else {
		*rest = NULL;
	}
	return f;
}

/* last is the rest of the line, which must not be empty. */
static char *last(char **rest, const char *what)
{
	if (*rest == NULL || **rest == '\0')
		fail("missing %s", what);
	char *f = *rest;
	*rest = NULL;
	return f;
}

static unsigned long number(const char *s, unsigned long max, const char *what)
{
	char *end;
	errno = 0;
	unsigned long n = strtoul(s, &end, 10);
	if (errno != 0 || *s == '\0' || *end != '\0' || n > max)
		fail("bad %s \"%s\"", what, s);
	return n;
}

static bool readonly(const char *s)
{
	if (strcmp(s, "ro") == 0)
		return true;
	if (strcmp(s, "rw") == 0)
		return false;
	fail("bad mode \"%s\": want ro or rw", s);
}

static void mac(const char *s, uint8_t out[6])
{
	unsigned int b[6];
	char extra;
	if (sscanf(s, "%2x:%2x:%2x:%2x:%2x:%2x%c", &b[0], &b[1], &b[2], &b[3], &b[4], &b[5], &extra) != 6)
		fail("bad MAC address \"%s\"", s);
	for (int i = 0; i < 6; i++)
		out[i] = (uint8_t)b[i];
}

int main(int argc, char **argv)
{
	if (argc == 2 && strcmp(argv[1], "--version") == 0) {
		/* Creating a context proves libkrun loaded and links. */
		int32_t ctx = krun_create_ctx();
		if (ctx < 0) {
			fprintf(stderr, "krun-launch: krun_create_ctx: %s\n", strerror(-ctx));
			return 1;
		}
		krun_free_ctx((uint32_t)ctx);
		printf("krun-launch %s\n", CONFIG_VERSION);
		return 0;
	}
	if (argc != 2) {
		fprintf(stderr, "usage: krun-launch --version | krun-launch CONFIG\n");
		return 2;
	}
	config_path = argv[1];
	FILE *f = fopen(config_path, "r");
	if (f == NULL)
		fail("open %s: %s", config_path, strerror(errno));

	check(krun_set_log_level(1), "krun_set_log_level");
	int32_t ret = krun_create_ctx();
	check(ret, "krun_create_ctx");
	uint32_t ctx = (uint32_t)ret;
	check(krun_disable_implicit_vsock(ctx), "krun_disable_implicit_vsock");
	check(krun_add_vsock(ctx, 0), "krun_add_vsock");

	unsigned long cpus = 0, memory = 0;
	char *kernel = NULL, *initrd = NULL, *cmdline = NULL;
	bool versioned = false;
	char *line = NULL;
	size_t cap = 0;
	ssize_t n;
	while ((n = getline(&line, &cap, f)) >= 0) {
		line_no++;
		if (n > 0 && line[n - 1] == '\n')
			line[--n] = '\0';
		if (n == 0)
			continue;
		char *rest = line;
		char *key = field(&rest, "keyword");
		if (!versioned) {
			if (strcmp(key, "krun-launch") != 0)
				fail("not a krun-launch configuration");
			char *v = last(&rest, "version");
			if (strcmp(v, CONFIG_VERSION) != 0)
				fail("configuration version %s, and this launcher reads %s", v, CONFIG_VERSION);
			versioned = true;
		} else if (strcmp(key, "cpus") == 0) {
			cpus = number(last(&rest, "CPUs"), 255, "CPU count");
		} else if (strcmp(key, "memory") == 0) {
			memory = number(last(&rest, "memory"), UINT32_MAX, "memory size");
		} else if (strcmp(key, "kernel") == 0) {
			kernel = strdup(last(&rest, "kernel path"));
		} else if (strcmp(key, "initrd") == 0) {
			initrd = strdup(last(&rest, "initrd path"));
		} else if (strcmp(key, "cmdline") == 0) {
			cmdline = strdup(last(&rest, "command line"));
		} else if (strcmp(key, "console") == 0) {
			check(krun_set_console_output(ctx, last(&rest, "console path")), "krun_set_console_output");
		} else if (strcmp(key, "disk") == 0) {
			char *id = field(&rest, "disk ID");
			bool ro = readonly(field(&rest, "disk mode"));
			check(krun_add_disk(ctx, id, last(&rest, "disk path"), ro), "krun_add_disk");
		} else if (strcmp(key, "vsock-in") == 0 || strcmp(key, "vsock-out") == 0) {
			uint32_t port = (uint32_t)number(field(&rest, "port"), UINT32_MAX, "port");
			bool listen = strcmp(key, "vsock-in") == 0;
			check(krun_add_vsock_port2(ctx, port, last(&rest, "socket path"), listen), "krun_add_vsock_port2");
		} else if (strcmp(key, "net") == 0) {
			int fd = (int)number(field(&rest, "descriptor"), INT32_MAX, "descriptor");
			uint8_t addr[6];
			mac(last(&rest, "MAC address"), addr);
			check(krun_add_net_unixstream(ctx, NULL, fd, addr, COMPAT_NET_FEATURES, 0), "krun_add_net_unixstream");
		} else if (strcmp(key, "share") == 0) {
			char *tag = field(&rest, "share tag");
			bool ro = readonly(field(&rest, "share mode"));
			check(krun_add_virtiofs3(ctx, tag, last(&rest, "share path"), 0, ro), "krun_add_virtiofs3");
		} else {
			fail("unknown keyword \"%s\"", key);
		}
		if (rest != NULL)
			fail("extra fields \"%s\"", rest);
	}
	if (ferror(f))
		fail("read %s: %s", config_path, strerror(errno));
	fclose(f);
	free(line);
	line_no = 0;
	if (!versioned)
		fail("%s is empty", config_path);
	if (kernel == NULL || cmdline == NULL || cpus == 0 || memory == 0)
		fail("%s names no kernel, command line, CPUs, or memory", config_path);

	check(krun_set_vm_config(ctx, (uint8_t)cpus, (uint32_t)memory), "krun_set_vm_config");
	check(krun_set_kernel(ctx, kernel, KRUN_KERNEL_FORMAT_ELF, initrd, cmdline), "krun_set_kernel");
	/* Returns only if the VM did not start. */
	check(krun_start_enter(ctx), "krun_start_enter");
	fail("krun_start_enter returned");
}
