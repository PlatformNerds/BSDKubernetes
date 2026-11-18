import lldb, tdbt
def ep(debugger, command, result, internal_dict):
    t = debugger.GetSelectedTarget(); p = t.GetProcess(); e = lldb.SBError()
    base = t.EvaluateExpression("(unsigned long)net_epoch_preempt->e_pcpu_record").GetValueAsUnsigned()
    print("e_pcpu_record=0x%x" % base)
    for cpu in range(8):
        rec = base + 4096 * cpu
        et = t.EvaluateExpression("(unsigned long)((struct epoch_record *)0x%x)->er_tdlist.tqh_first" % rec).GetValueAsUnsigned()
        cpuid = t.EvaluateExpression("((struct epoch_record *)0x%x)->er_cpuid" % rec).GetValueAsUnsigned()
        n = 0
        while et and n < 20:
            n += 1
            td = t.EvaluateExpression("(unsigned long)((struct epoch_tracker *)0x%x)->et_td" % et).GetValueAsUnsigned()
            comm = t.EvaluateExpression("((struct thread *)0x%x)->td_proc->p_comm" % td).GetSummary()
            pid = t.EvaluateExpression("((struct thread *)0x%x)->td_proc->p_pid" % td).GetValueAsSigned()
            tid = t.EvaluateExpression("((struct thread *)0x%x)->td_tid" % td).GetValueAsSigned()
            inh = t.EvaluateExpression("((struct thread *)0x%x)->td_inhibitors" % td).GetValueAsUnsigned()
            st = t.EvaluateExpression("((struct thread *)0x%x)->td_state" % td).GetValueAsUnsigned()
            blk = t.EvaluateExpression("(unsigned long)((struct thread *)0x%x)->td_blocked" % td).GetValueAsUnsigned()
            ln = t.EvaluateExpression("((struct thread *)0x%x)->td_lockname" % td).GetSummary()
            wm = t.EvaluateExpression("((struct thread *)0x%x)->td_wmesg" % td).GetSummary()
            print("cpu %d (rec cpuid %d): tracker 0x%x td 0x%x pid %d %s tid %d state %d inhib 0x%x blocked 0x%x lockname %s wmesg %s" % (cpu, cpuid, et, td, pid, comm, tid, st, inh, blk, ln, wm))
            debugger.HandleCommand("tdbt 0x%x" % td)
            et = t.EvaluateExpression("(unsigned long)((struct epoch_tracker *)0x%x)->et_link.tqe_next" % et).GetValueAsUnsigned()
def __lldb_init_module(debugger, internal_dict):
    debugger.HandleCommand("command script add -f ep.ep ep")
