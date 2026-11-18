/*
 * rmepoch_test: deterministic reproducer for the rmlock lost wakeup of
 * research/21 (freebsd-src patch 0032).
 *
 * The pattern is the one pf creates on every interface attach/detach:
 *
 *	R  holds a tracked read lock on an rmlock (a packet in pf_test()).
 *	W  enters a preemptible epoch and write-locks the rmlock
 *	   (pfi_attach_ifnet_event()); it sleeps on the rmlock's turnstile
 *	   with R as owner.
 *	E1, E2 call epoch_wait_preempt() on that epoch (if_alloc_domain(),
 *	   if_detach_internal()); epoch_block_handler_preempt() finds W
 *	   blocked on a lock and puts them on the same turnstile.
 *
 * Then R unlocks.  _rm_unlock_hard() used turnstile_signal(), which wakes
 * one thread; whoever is left sleeps on a turnstile nothing will signal
 * again.  The test reports who finished and, if anything is stuck, wakes
 * the turnstile itself (what the fix does) so the module can unload.
 *
 *	kldload ./rmepoch_test.ko
 *	sysctl debug.rmepoch_test.run=1		# result in dmesg
 *	sysctl debug.rmepoch_test.result	# "ok ..." or "STUCK ..."
 */

#include <sys/param.h>
#include <sys/systm.h>
#include <sys/kernel.h>
#include <sys/kthread.h>
#include <sys/lock.h>
#include <sys/module.h>
#include <sys/mutex.h>
#include <sys/proc.h>
#include <sys/rmlock.h>
#include <sys/sched.h>
#include <sys/sx.h>
#include <sys/sysctl.h>
#include <sys/turnstile.h>
#include <sys/epoch.h>
#include <machine/atomic.h>
#include <machine/cpu.h>

#define	NWAITERS	2

static struct rmlock	t_rm;
static epoch_t		t_epoch;
static volatile int	r_holding, r_release, r_done;
static volatile int	w_entered, w_done;
static volatile int	e_started, e_done;
static struct thread	*r_td, *w_td, *e_td[NWAITERS];
static char		t_result[160] = "not run";
static struct sx	t_sx;

static void
t_reader(void *arg __unused)
{
	struct rm_priotracker tr;

	rm_rlock(&t_rm, &tr);
	atomic_store_rel_int(&r_holding, 1);
	while (atomic_load_acq_int(&r_release) == 0)
		cpu_spinwait();
	rm_runlock(&t_rm, &tr);
	atomic_store_rel_int(&r_done, 1);
	kthread_exit();
}

static void
t_writer(void *arg __unused)
{
	struct epoch_tracker et;

	epoch_enter_preempt(t_epoch, &et);
	atomic_store_rel_int(&w_entered, 1);
	rm_wlock(&t_rm);
	rm_wunlock(&t_rm);
	epoch_exit_preempt(t_epoch, &et);
	atomic_store_rel_int(&w_done, 1);
	kthread_exit();
}

static void
t_waiter(void *arg __unused)
{

	atomic_add_int(&e_started, 1);
	epoch_wait_preempt(t_epoch);
	atomic_add_int(&e_done, 1);
	kthread_exit();
}

static bool
t_on_lock(struct thread *td)
{
	bool on;

	thread_lock(td);
	on = TD_ON_LOCK(td);
	thread_unlock(td);
	return (on);
}

/* Wait up to ms milliseconds for cond; evaluates to its final value. */
#define	WAIT_FOR(cond, ms) ({						\
	int _i;								\
	for (_i = 0; _i < (ms) / 10 && !(cond); _i++)			\
		pause("rmet", hz / 100 + 1);				\
	(bool)(cond);							\
})

/* What the fix does: wake every thread on the rmlock's turnstile. */
static int
t_wake_all(void)
{
	struct turnstile *ts;
	int woke = 0;

	turnstile_chain_lock(&t_rm.lock_object);
	ts = turnstile_lookup(&t_rm.lock_object);
	if (ts != NULL) {
		turnstile_broadcast(ts, TS_EXCLUSIVE_QUEUE);
		turnstile_unpend(ts);
		woke = 1;
	} else
		turnstile_chain_unlock(&t_rm.lock_object);
	if (woke)
		turnstile_chain_unlock(&t_rm.lock_object);
	return (woke);
}

static bool
t_all_done(void)
{

	return (r_done && w_done && e_done == e_started);
}

static int
t_run(void)
{
	int i, wdone, edone, rescued = 0;

	r_holding = r_release = r_done = 0;
	w_entered = w_done = 0;
	e_started = e_done = 0;
	r_td = w_td = NULL;

	rm_init(&t_rm, "rmepoch_test");
	t_epoch = epoch_alloc("rmepoch_test", EPOCH_PREEMPT);

	kthread_add(t_reader, NULL, NULL, &r_td, 0, 0, "rmet_reader");
	if (!WAIT_FOR(atomic_load_acq_int(&r_holding), 2000)) {
		snprintf(t_result, sizeof(t_result), "setup: reader");
		goto out;
	}
	kthread_add(t_writer, NULL, NULL, &w_td, 0, 0, "rmet_writer");
	if (!WAIT_FOR(w_entered && t_on_lock(w_td), 2000)) {
		snprintf(t_result, sizeof(t_result),
		    "setup: writer never blocked");
		goto out;
	}
	for (i = 0; i < NWAITERS; i++)
		kthread_add(t_waiter, NULL, NULL, &e_td[i], 0, 0,
		    "rmet_waiter%d", i);
	/* Both waiters must be parked on the rmlock's turnstile. */
	if (!WAIT_FOR(e_started == NWAITERS && t_on_lock(e_td[0]) &&
	    t_on_lock(e_td[1]), 3000)) {
		snprintf(t_result, sizeof(t_result),
		    "setup: waiters not on the turnstile (started %d)",
		    e_started);
		goto out;
	}

	/* The last tracked reader leaves. */
	atomic_store_rel_int(&r_release, 1);
	WAIT_FOR(r_done && w_done && e_done == NWAITERS, 3000);
	wdone = w_done;
	edone = e_done;
	if (wdone && edone == NWAITERS)
		snprintf(t_result, sizeof(t_result),
		    "ok: writer and %d epoch waiters finished", NWAITERS);
	else
		snprintf(t_result, sizeof(t_result),
		    "STUCK: writer %s, %d of %d epoch waiters finished 3s "
		    "after the last reader left", wdone ? "done" : "asleep",
		    edone, NWAITERS);

out:
	/*
	 * Unblock anything still asleep so every thread exits.  A thread
	 * woken here rechecks and may block again, so repeat.
	 */
	atomic_store_rel_int(&r_release, 1);
	for (i = 0; i < 100 && !t_all_done(); i++) {
		rescued += t_wake_all();
		pause("rmet", hz / 10);
	}
	if (!t_all_done()) {
		/* Leak rather than free state that threads still use. */
		printf("rmepoch_test: %s; threads still stuck, not freeing\n",
		    t_result);
		return (EBUSY);
	}
	/* Let the kthreads finish exiting. */
	pause("rmet", hz / 5);
	epoch_free(t_epoch);
	rm_destroy(&t_rm);
	printf("rmepoch_test: %s%s\n", t_result, rescued ?
	    " (left-over sleepers woken by a turnstile broadcast)" : "");
	return (0);
}

static int
sysctl_run(SYSCTL_HANDLER_ARGS)
{
	int error, v = 0;

	error = sysctl_handle_int(oidp, &v, 0, req);
	if (error != 0 || req->newptr == NULL || v == 0)
		return (error);
	sx_xlock(&t_sx);
	error = t_run();
	sx_xunlock(&t_sx);
	return (error);
}

static SYSCTL_NODE(_debug, OID_AUTO, rmepoch_test,
    CTLFLAG_RW | CTLFLAG_MPSAFE, 0, "rmlock/epoch lost wakeup test");
SYSCTL_PROC(_debug_rmepoch_test, OID_AUTO, run,
    CTLTYPE_INT | CTLFLAG_RW | CTLFLAG_MPSAFE, NULL, 0, sysctl_run, "I",
    "Run the test");
SYSCTL_STRING(_debug_rmepoch_test, OID_AUTO, result, CTLFLAG_RD, t_result,
    0, "Result of the last run");

static int
t_modevent(module_t mod __unused, int type, void *data __unused)
{

	switch (type) {
	case MOD_LOAD:
		sx_init(&t_sx, "rmepoch_test");
		return (0);
	case MOD_UNLOAD:
		sx_destroy(&t_sx);
		return (0);
	default:
		return (EOPNOTSUPP);
	}
}

static moduledata_t t_mod = { "rmepoch_test", t_modevent, NULL };
DECLARE_MODULE(rmepoch_test, t_mod, SI_SUB_KLD, SI_ORDER_ANY);
MODULE_VERSION(rmepoch_test, 1);
