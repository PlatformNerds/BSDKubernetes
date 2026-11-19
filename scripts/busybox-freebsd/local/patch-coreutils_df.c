--- coreutils/df.c.orig	2026-11-18 19:54:15
+++ coreutils/df.c	2026-11-18 19:55:56
@@ -97,10 +97,74 @@
 //usage:       "Filesystem          512-blocks      Used Available Capacity Mounted on\n"
 //usage:       "/dev/sda3             17381728  17107080    274648      98% /\n"
 
-#include <mntent.h>
+#if defined(__FreeBSD__)
+# include <sys/param.h>
+# include <sys/ucred.h>
+# include <sys/mount.h>
+#else
+# include <mntent.h>
+#endif
 #include <sys/statvfs.h>
 #include "libbb.h"
 #include "unicode.h"
+
+#if defined(__FreeBSD__)
+/*
+ * FreeBSD has no mount table file (Linux's /proc/mounts) and no <mntent.h>:
+ * walk getmntinfo(3) instead, and find a path's mount with statfs(2).
+ * Mounts flagged MNT_IGNORE (e.g. a container's volumes) are listed like
+ * any other, as Linux lists bind mounts.
+ */
+struct mntent {
+	char *mnt_fsname;
+	char *mnt_dir;
+	char *mnt_type;
+};
+static struct statfs *bsd_mnt;
+static int bsd_nmnt, bsd_idx;
+static struct statfs bsd_sfs;
+static struct mntent bsd_ent;
+
+static struct mntent *bsd_fill(struct statfs *f)
+{
+	bsd_ent.mnt_fsname = f->f_mntfromname;
+	/*
+	 * In a jail, a nullfs mount's source is a host path the jail can't
+	 * see (a container's volumes): name it by its type, as Linux names a
+	 * bind mount by its source filesystem rather than by a path. This
+	 * also keeps the line from wrapping, which scripts that read the
+	 * size with awk '{print $2}' rely on.
+	 */
+	if (strcmp(f->f_fstypename, "nullfs") == 0 && f->f_mntfromname[0] == '/'
+	 && access(f->f_mntfromname, F_OK) != 0)
+		bsd_ent.mnt_fsname = f->f_fstypename;
+	bsd_ent.mnt_dir = f->f_mntonname;
+	bsd_ent.mnt_type = f->f_fstypename;
+	return &bsd_ent;
+}
+static FILE *setmntent(const char *file UNUSED_PARAM, const char *mode UNUSED_PARAM)
+{
+	bsd_nmnt = getmntinfo(&bsd_mnt, MNT_NOWAIT);
+	bsd_idx = 0;
+	return bsd_nmnt > 0 ? (FILE *)bsd_mnt : NULL;
+}
+static struct mntent *getmntent(FILE *fp UNUSED_PARAM)
+{
+	if (bsd_idx >= bsd_nmnt)
+		return NULL;
+	return bsd_fill(&bsd_mnt[bsd_idx++]);
+}
+static int endmntent(FILE *fp UNUSED_PARAM)
+{
+	return 1;
+}
+static struct mntent *find_mount_point(const char *name, int subdir_too UNUSED_PARAM)
+{
+	if (statfs(name, &bsd_sfs) != 0)
+		return NULL;
+	return bsd_fill(&bsd_sfs);
+}
+#endif
 
 #if !ENABLE_FEATURE_HUMAN_READABLE
 static unsigned long kscale(unsigned long b, unsigned long bs)
