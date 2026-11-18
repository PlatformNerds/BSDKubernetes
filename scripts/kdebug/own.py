import lldb, tdbt
def own(debugger, command, result, internal_dict):
    t = debugger.GetSelectedTarget()
    for arg in command.split():
        ts = t.EvaluateExpression("(unsigned long)((struct thread *)%s)->td_blocked" % arg).GetValueAsUnsigned()
        print("td %s td_blocked=0x%x" % (arg, ts))
        if not ts: continue
        lo = t.EvaluateExpression("(unsigned long)((struct turnstile *)0x%x)->ts_lockobj" % ts).GetValueAsUnsigned()
        nm = t.EvaluateExpression("((struct lock_object *)0x%x)->lo_name" % lo).GetSummary()
        cls = t.EvaluateExpression("(unsigned)((struct lock_object *)0x%x)->lo_flags" % lo).GetValueAsUnsigned()
        word = t.EvaluateExpression("(unsigned long)((struct mtx *)0x%x)->mtx_lock" % lo).GetValueAsUnsigned()
        owner = t.EvaluateExpression("(unsigned long)((struct turnstile *)0x%x)->ts_owner" % ts).GetValueAsUnsigned()
        print("  lock_object=0x%x name=%s flags=0x%x lockword=0x%x ts_owner=0x%x" % (lo, nm, cls, word, owner))
def __lldb_init_module(debugger, internal_dict):
    debugger.HandleCommand("command script add -f own.own own")
