import lldb, tdbt
def alltd(debugger, command, result, internal_dict):
    t = debugger.GetSelectedTarget(); p = t.GetProcess(); e = lldb.SBError()
    pr = t.EvaluateExpression("allproc.lh_first")
    out = []
    while pr.GetValueAsUnsigned():
        d = pr.Dereference()
        comm = d.GetChildMemberWithName("p_comm").GetSummary()
        pid = d.GetChildMemberWithName("p_pid").GetValueAsSigned()
        td = d.GetChildMemberWithName("p_threads").GetChildMemberWithName("tqh_first")
        while td.GetValueAsUnsigned():
            tdv = td.Dereference()
            pcb = tdv.GetChildMemberWithName("td_pcb").GetValueAsUnsigned()
            wm = tdv.GetChildMemberWithName("td_wmesg")
            wms = wm.GetSummary() if wm.GetValueAsUnsigned() else ""
            fp = p.ReadUnsignedFromMemory(pcb + 10*8, 8, e)
            lr = tdbt.strip(p.ReadUnsignedFromMemory(pcb + 11*8, 8, e))
            frames = []
            for i in range(18):
                frames.append(tdbt.sym(t, lr).split(" ")[0])
                if fp == 0: break
                nfp = p.ReadUnsignedFromMemory(fp, 8, e); lr = tdbt.strip(p.ReadUnsignedFromMemory(fp+8, 8, e))
                if not e.Success() or lr == 0: break
                fp = nfp
            s = " < ".join(frames)
            if any(k in s for k in ("pf", "rm_", "_rm_", "epoch", "pfi", "ifnet", "if_", "allproc", "prison")):
                print("TD pid=%d %s td=0x%x wmesg=%s :: %s" % (pid, comm, td.GetValueAsUnsigned(), wms, s))
            td = tdv.GetChildMemberWithName("td_plist").GetChildMemberWithName("tqe_next")
        pr = d.GetChildMemberWithName("p_list").GetChildMemberWithName("le_next")
def __lldb_init_module(debugger, internal_dict):
    debugger.HandleCommand("command script add -f alltd.alltd alltd")
