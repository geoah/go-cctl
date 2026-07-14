// cctl — session sidebar (read-only over cmux's live `workspaces` context).
//
// Row anatomy mirrors cmux's default sidebar: title, latest agent message,
// "⎇ branch • path" meta line, PR line — but nested repo → worktree → session
// like the cctl TUI. Selected session = filled card (only that one). The cctl
// control workspace is a distinct padded block with a divider.
VStack(alignment: .leading, spacing: 4) {
    let items = workspaces.sorted { $0.title < $1.title }
    ForEach(items.indices) { i in
        let w = items[i]
        let parts = w.title.split(separator: "/")
        let isSession = parts.count > 2
        let isControl = parts.count < 2
        let repo = parts.count > 0 ? String(parts[0]) : w.title
        let wt = parts.count > 1 ? String(parts[1]) : ""
        let sess = isSession ? String(parts[2]) : w.title

        let prev = i > 0 ? items[i - 1].title.split(separator: "/") : []
        let prevRepo = prev.count > 0 ? String(prev[0]) : ""
        let prevWt = prev.count > 1 ? String(prev[1]) : ""

        let rowBg = w.selected ? "#2E5FBF" : "#00000000"
        let nameColor = w.selected ? "#FFFFFF" : "#E6EAF0"
        let metaColor = w.selected ? "#C9D8F5" : "#6B7688"

        if isControl {
            // ── control block ──
            Text(" ").font(.caption)
            Button(action: { cmux("workspace.select", workspace_id: w.id) }) {
                VStack(alignment: .leading, spacing: 1) {
                    HStack(spacing: 7) {
                        Text("cctl").font(.title3).bold().foregroundColor(w.selected ? "#86EFAC" : "#F0F3F7")
                        Text("control center").font(.caption).foregroundColor("#6B7688")
                        Spacer()
                        if w.unread > 0 { Text("\(w.unread)●").font(.caption).foregroundColor("#4C9AFF") }
                    }
                    if let m = w.latestMessage {
                        Text(m).font(.caption).foregroundColor("#626C7D").lineLimit(1)
                    }
                }
            }
            Text(" ").font(.caption)
            Divider()
        } else {
            // h1: repo.
            if repo != prevRepo {
                if i > 0 { Text(" ").font(.caption) }
                Text(repo).font(.title3).bold().foregroundColor("#CBD3DF")
            }
            // h2: worktree.
            if isSession && (repo != prevRepo || wt != prevWt) {
                Text("  " + wt).font(.callout).bold().foregroundColor("#717D90")
            }
            // h3: session card.
            HStack(spacing: 0) {
                Text("    ")
                VStack(alignment: .leading, spacing: 2) {
                        HStack(spacing: 6) {
                            Text(sess).font(.body).bold().foregroundColor(nameColor)
                            if w.dirty == true { Text("±").font(.body).foregroundColor("#E0A800") }
                            Spacer()
                            if let p = w.progress {
                                Text(p.label != "" ? p.label : "\(Int(p.value * 100))%")
                                    .font(.caption).foregroundColor("#F5A623")
                            }
                            if w.unread > 0 { Text("\(w.unread)●").font(.caption).foregroundColor("#4C9AFF") }
                            if let a = w.latestAt {
                                let mins = Int((clock.epoch - a) / 60)
                                Text(mins < 1 ? "now" : (mins < 60 ? "\(mins)m" : "\(mins / 60)h"))
                                    .font(.caption).foregroundColor(metaColor)
                            }
                        }
                        if let m = w.latestMessage {
                            Text(m).font(.caption).foregroundColor(w.selected ? "#DCE7FA" : "#8B95A7").lineLimit(1)
                        }
                        // "~"-shorten the home prefix so paths read compactly.
                        let dir = w.directory.hasPrefix("/Users/") && w.directory.split(separator: "/").count > 2
                            ? "~/" + w.directory.split(separator: "/").dropFirst(2).joined(separator: "/")
                            : w.directory
                        Text((w.branch != nil ? "⎇ " + w.branch! + "  •  " : "") + dir)
                            .font(.caption).foregroundColor(metaColor).lineLimit(1)
                        if let pr = w.pr {
                            HStack(spacing: 5) {
                                Text("PR #\(pr.number)").font(.caption).bold()
                                    .foregroundColor(w.selected ? "#FFFFFF" : "#A78BDA")
                                Text(pr.status).font(.caption)
                                    .foregroundColor(pr.status == "open" ? "#3FB950" : (w.selected ? "#DCE7FA" : "#8B95A7"))
                            }
                        }
                    }
                    .padding(5)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(rowBg)
                    .cornerRadius(6)
                    // Whole-card click: onTapGesture hit-tests the full view
                    // bounds (padding + background), unlike Button which only
                    // registered on the text glyphs.
                    .onTapGesture { cmux("workspace.select", workspace_id: w.id) }
                // Right-click actions. Note: "close" only closes the cmux
                // tab — the session stays in cctl's manifest, so the next
                // reconcile revives it (a true delete is dd in the cctl TUI).
                .contextMenu {
                    Button(action: { cmux("workspace.select", workspace_id: w.id) }) {
                        Text("Focus")
                    }
                    Button(action: { cmux("workspace.close", workspace_id: w.id) }) {
                        Text("Close tab (revives on next reconcile)")
                    }
                }
            }
        }
    }
}
