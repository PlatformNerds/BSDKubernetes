--- util-linux/mount.c.orig	2026-11-20 18:03:00
+++ util-linux/mount.c	2026-11-20 18:04:01
@@ -181,6 +181,200 @@
 //usage:#define mount_notes_usage
 //usage:       "Returns 0 for success, number of failed mounts for -a, or errno for one mount."
 
+#if defined(__FreeBSD__)
+/*
+ * FreeBSD port. The applet below is Linux-only (mount(2) with MS_* flags,
+ * <mntent.h>, /etc/mtab), so on FreeBSD:
+ *
+ * Listing shows the table Linux's mount reads, /proc/mounts, in its
+ * format: linprocfs provides it (a Kubernetes container runtime on
+ * FreeBSD mounts linprocfs on /proc), and it describes nullfs (bind)
+ * mounts by the filesystem behind them, as Linux does. Without it, the
+ * table comes from getmntinfo(3).
+ *
+ * Mounting works like FreeBSD's mount(8): mount_<type> runs if there is
+ * one (nfs, nullfs, ...), otherwise nmount(2) gets the options as
+ * name[=value] pairs. "bind" and "rbind" mean nullfs, "remount" is
+ * FreeBSD's "update".
+ */
+#include <sys/param.h>
+#include <sys/mount.h>
+#include <sys/uio.h>
+#include "libbb.h"
+
+static void bsd_list_proc(FILE *fp, const char *type)
+{
+	char line[4096], from[1024], on[1024], fstype[64], opts[1024];
+
+	while (fgets(line, sizeof(line), fp)) {
+		if (sscanf(line, "%1023s %1023s %63s %1023s",
+		    from, on, fstype, opts) != 4)
+			continue;
+		if (!type || strcmp(fstype, type) == 0)
+			printf("%s on %s type %s (%s)\n", from, on, fstype, opts);
+	}
+}
+
+static void bsd_list_getmntinfo(const char *type)
+{
+	static const struct { uint64_t flag; const char *name; } fl[] = {
+		{ MNT_SYNCHRONOUS, "sync" }, { MNT_NOEXEC, "noexec" },
+		{ MNT_NOSUID, "nosuid" }, { MNT_UNION, "union" },
+		{ MNT_ASYNC, "async" }, { MNT_NOSYMFOLLOW, "nosymfollow" },
+		{ MNT_NOATIME, "noatime" },
+	};
+	struct statfs *m;
+	int i, n;
+	unsigned j;
+
+	n = getmntinfo(&m, MNT_NOWAIT);
+	if (n <= 0)
+		bb_simple_perror_msg_and_die("getmntinfo");
+	for (i = 0; i < n; i++) {
+		if (type && strcmp(m[i].f_fstypename, type) != 0)
+			continue;
+		printf("%s on %s type %s (%s", m[i].f_mntfromname,
+		    m[i].f_mntonname, m[i].f_fstypename,
+		    (m[i].f_flags & MNT_RDONLY) ? "ro" : "rw");
+		for (j = 0; j < ARRAY_SIZE(fl); j++)
+			if (m[i].f_flags & fl[j].flag)
+				printf(",%s", fl[j].name);
+		printf(")\n");
+	}
+}
+
+static int bsd_find_helper(const char *type, char *path, size_t size)
+{
+	static const char *const dirs[] = { "/sbin", "/usr/sbin" };
+	unsigned i;
+
+	for (i = 0; i < ARRAY_SIZE(dirs); i++) {
+		snprintf(path, size, "%s/mount_%s", dirs[i], type);
+		if (access(path, X_OK) == 0)
+			return 1;
+	}
+	return 0;
+}
+
+static void bsd_iov(struct iovec **iov, int *n, const char *name,
+    void *val, size_t len)
+{
+	*iov = xrealloc(*iov, sizeof(**iov) * (*n + 2));
+	(*iov)[*n].iov_base = xstrdup(name);
+	(*iov)[*n].iov_len = strlen(name) + 1;
+	(*iov)[*n + 1].iov_base = val;
+	(*iov)[*n + 1].iov_len = len;
+	*n += 2;
+}
+
+static char *bsd_append(char *list, const char *opt)
+{
+	return xasprintf("%s%s%s", list, *list ? "," : "", opt);
+}
+
+int mount_main(int argc, char **argv) MAIN_EXTERNALLY_VISIBLE;
+int mount_main(int argc UNUSED_PARAM, char **argv)
+{
+	enum { OPT_o = 1 << 0, OPT_t = 1 << 1, OPT_r = 1 << 2, OPT_w = 1 << 3,
+	    OPT_v = 1 << 4, OPT_f = 1 << 5, OPT_n = 1 << 6, OPT_i = 1 << 7 };
+	llist_t *lst_o = NULL;
+	char *type = NULL, *opts, *kept, *o, *tok, *val;
+	char helper[MAXPATHLEN], errmsg[255];
+	struct iovec *iov = NULL;
+	int iovlen = 0, i, j;
+	unsigned opt;
+	FILE *fp;
+
+	/* --bind, --rbind, ...: options, as in the Linux applet */
+	opts = xzalloc(1);
+	for (i = j = 1; argv[i]; i++) {
+		if (argv[i][0] == '-' && argv[i][1] == '-')
+			opts = bsd_append(opts, argv[i] + 2);
+		else
+			argv[j++] = argv[i];
+	}
+	argv[j] = NULL;
+
+	opt = getopt32(argv, "^" "o:*t:rwvfni" "\0" "?2", &lst_o, &type);
+	while (lst_o)
+		opts = bsd_append(opts, llist_pop(&lst_o));
+	if (opt & OPT_r)
+		opts = bsd_append(opts, "ro");
+	if (opt & OPT_w)
+		opts = bsd_append(opts, "rw");
+	argv += optind;
+
+	if (!argv[0]) {
+		/* Split, so the port's "/proc -> /compat/linux/proc" edit keeps it. */
+		fp = fopen("/" "proc/mounts", "r");
+		if (!fp)
+			fp = fopen("/compat/linux/proc/mounts", "r");
+		if (fp) {
+			bsd_list_proc(fp, type);
+			fclose(fp);
+		} else
+			bsd_list_getmntinfo(type);
+		return EXIT_SUCCESS;
+	}
+	if (!argv[1])
+		bb_simple_error_msg_and_die("mounting from /etc/fstab is not "
+		    "supported: give DEVICE and NODE");
+
+	kept = xzalloc(1);
+	for (o = opts; (tok = strsep(&o, ",")) != NULL; ) {
+		if (*tok == '\0' || strcmp(tok, "defaults") == 0)
+			continue;
+		if (strcmp(tok, "bind") == 0 || strcmp(tok, "rbind") == 0)
+			type = (char *)"nullfs";
+		else if (strcmp(tok, "remount") == 0)
+			kept = bsd_append(kept, "update");
+		else
+			kept = bsd_append(kept, tok);
+	}
+	if (!type)
+		type = (char *)(strchr(argv[0], ':') ? "nfs" : "ufs");
+	if (opt & OPT_f)
+		return EXIT_SUCCESS;
+
+	if (!(opt & OPT_i) && bsd_find_helper(type, helper, sizeof(helper))) {
+		char *args[6];
+		int k = 0;
+
+		args[k++] = xasprintf("mount_%s", type);
+		if (*kept) {
+			args[k++] = (char *)"-o";
+			args[k++] = kept;
+		}
+		args[k++] = argv[0];
+		args[k++] = argv[1];
+		args[k] = NULL;
+		execv(helper, args);
+		bb_perror_msg_and_die("can't execute '%s'", helper);
+	}
+
+	bsd_iov(&iov, &iovlen, "fstype", type, strlen(type) + 1);
+	bsd_iov(&iov, &iovlen, "fspath", argv[1], strlen(argv[1]) + 1);
+	bsd_iov(&iov, &iovlen, "from", argv[0], strlen(argv[0]) + 1);
+	errmsg[0] = '\0';
+	bsd_iov(&iov, &iovlen, "errmsg", errmsg, sizeof(errmsg));
+	for (o = kept; (tok = strsep(&o, ",")) != NULL; ) {
+		if (*tok == '\0')
+			continue;
+		val = strchr(tok, '=');
+		if (val)
+			*val++ = '\0';
+		bsd_iov(&iov, &iovlen, tok, val, val ? strlen(val) + 1 : 0);
+	}
+	if (nmount(iov, iovlen, 0) != 0) {
+		if (errmsg[0])
+			bb_error_msg_and_die("mounting %s on %s failed: %s: %s",
+			    argv[0], argv[1], errmsg, strerror(errno));
+		bb_perror_msg_and_die("mounting %s on %s failed",
+		    argv[0], argv[1]);
+	}
+	return EXIT_SUCCESS;
+}
+#else
 #include <mntent.h>
 #if ENABLE_FEATURE_SYSLOG
 #include <syslog.h>
@@ -2549,3 +2743,4 @@
 //64 some mount succeeded
 	return rc;
 }
+#endif /* __FreeBSD__ */
