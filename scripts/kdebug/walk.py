import lldb
def cs(v):
    try:
        e = lldb.SBError(); b = v.GetData().ReadRawData(e, 0, v.GetByteSize())
        return b.split(b"\0")[0].decode(errors="replace")
    except Exception: return "?"
def walk(debugger, command, result, internal_dict):
    t = debugger.GetSelectedTarget()
    p = t.EvaluateExpression("allproc.lh_first")
    n = 0
    while p.GetValueAsUnsigned() != 0 and n < 2000:
        n += 1
        pid = p.Dereference().GetChildMemberWithName("p_pid").GetValueAsSigned()
        comm = cs(p.Dereference().GetChildMemberWithName("p_comm"))
        td = p.Dereference().GetChildMemberWithName("p_threads").GetChildMemberWithName("tqh_first")
        while td.GetValueAsUnsigned() != 0:
            d = td.Dereference()
            wm = d.GetChildMemberWithName("td_wmesg")
            wmesg = wm.GetSummary() if wm.GetValueAsUnsigned() else ""
            lk = d.GetChildMemberWithName("td_lockname")
            lname = lk.GetSummary() if lk.GetValueAsUnsigned() else ""
            st = d.GetChildMemberWithName("td_state").GetValueAsUnsigned()
            if wmesg or lname:
                print("pid=%d %-16s td=0x%x tid=%d state=%d wmesg=%s lock=%s" % (pid, comm, td.GetValueAsUnsigned(), d.GetChildMemberWithName("td_tid").GetValueAsSigned(), st, wmesg, lname))
            td = d.GetChildMemberWithName("td_plist").GetChildMemberWithName("tqe_next")
        p = p.Dereference().GetChildMemberWithName("p_list").GetChildMemberWithName("le_next")
    print("procs walked:", n)
def __lldb_init_module(debugger, internal_dict):
    debugger.HandleCommand("command script add -f walk.walk walkprocs")
