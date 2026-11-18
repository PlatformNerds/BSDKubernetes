import lldb
def strip(a):
    return (a & 0x0000ffffffffffff) | 0xffff000000000000 if a >> 48 else a
def sym(t, a):
    sa = t.ResolveLoadAddress(a)
    f = sa.GetFunction()
    if f.IsValid():
        le = sa.GetLineEntry()
        return "%s %s:%d" % (f.GetName(), le.GetFileSpec().GetFilename(), le.GetLine())
    s = sa.GetSymbol()
    if s.IsValid(): return s.GetName()
    return "0x%x" % a
def tdbt(debugger, command, result, internal_dict):
    t = debugger.GetSelectedTarget(); p = t.GetProcess(); e = lldb.SBError()
    for arg in command.split():
        td = int(arg, 0)
        pcb = t.EvaluateExpression("(unsigned long)((struct thread *)%s)->td_pcb" % arg).GetValueAsUnsigned()
        comm = t.EvaluateExpression("((struct thread *)%s)->td_proc->p_comm" % arg).GetSummary()
        wm = t.EvaluateExpression("((struct thread *)%s)->td_wmesg" % arg).GetSummary()
        print("== td %s %s wmesg=%s" % (arg, comm, wm))
        fp = p.ReadUnsignedFromMemory(pcb + 10*8, 8, e)
        lr = strip(p.ReadUnsignedFromMemory(pcb + 11*8, 8, e))
        for i in range(25):
            print("  #%d %s" % (i, sym(t, lr)))
            if fp == 0: break
            nfp = p.ReadUnsignedFromMemory(fp, 8, e); lr = strip(p.ReadUnsignedFromMemory(fp + 8, 8, e))
            if not e.Success() or lr == 0: break
            fp = nfp
def kldlist(debugger, command, result, internal_dict):
    t = debugger.GetSelectedTarget()
    lf = t.EvaluateExpression("linker_files.tqh_first")
    while lf.GetValueAsUnsigned():
        d = lf.Dereference()
        print("kld %s 0x%x" % (d.GetChildMemberWithName("filename").GetSummary(), d.GetChildMemberWithName("address").GetValueAsUnsigned()))
        lf = d.GetChildMemberWithName("link").GetChildMemberWithName("tqe_next")
def __lldb_init_module(debugger, internal_dict):
    debugger.HandleCommand("command script add -f tdbt.tdbt tdbt")
    debugger.HandleCommand("command script add -f tdbt.kldlist kldlist")
