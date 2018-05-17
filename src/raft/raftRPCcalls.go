package raft

/*----------------------------------------------------------*/
/*this files is for the leader to send RPC call to each Raft*/
/*----------------------------------------------------------*/

//AppendEntries apply log entries from leader
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	reply.Success = false

	//old leader "sending" args
	//raft with recognization of the new leader "receive" this args
	//the old leader(failure or delay so there is a another true leader now)
	//the old leader send the request args with lesser than follower's currentTerm
	//just return the upToDate term to the fake old leader
	if args.Term < rf.currentTerm {
		reply.Term = rf.currentTerm
		reply.NextTryIndex = rf.getLastLogIndex() + 1
		return
	}

	//new leader first time "sending" heartbeat to follower/candidate who is normal or wake up from a failure/delay
	//now the rf is a follower or candidator
	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.status = Follower
		rf.votedFor = -1
	}

	//using the heartbeat channel to pass the heartbeat message to raft runServer() goroutine
	rf.heartbeat <- true
	reply.Term = rf.currentTerm

	//if the logs from leaders is incomplete for the current raft than return the reply to get a complete logs for current raft
	//return the failure reply for leader so the leader will decrement nextIndex and retry
	//add the nextIndex to optimize the retry times
	/*	incomplete: leader is trying to append index-6 but the follower last logs index is 2
		index:			012345
		leader-logs:	112223
		follow-logs:	112
	*/
	//set the prevLogIndex to the nextIndex of rf.logs
	if args.PrevLogIndex > rf.getLastLogIndex() {
		reply.NextTryIndex = rf.getLastLogIndex() + 1
		return
	}

	//now the args.prevLogIndex == rf.getLastLogIndex()+1, and the leader sending appendEntries of [] because the
	/*
			index			0123456
			leader-logs:l1:	012
			follow-logs:l2:	011111
		the if-block below detect whether the l1[3].term == l2[3].term
		if equal than only needs to replicate the succeeding logEntries
		if unequal than needs more logEntires to replicate
	*/
	if args.PrevLogIndex > 0 && rf.logs[args.PrevLogIndex].Term != args.PrevLogTerm {
		//needs more logEntires to replicate
		//in this case the X == 2
		/*for example
		index			0123456
		leader-logs:l1:	1133445
		follow-logs:l2:	1122
		*/
		//so the leader needs to find out the previous conflicted term\index
		//args.PrevLogTerm ==
		//args.PrevLogIndex == 2
		//term == 1
		term := rf.logs[args.PrevLogIndex].Term

		//to repeatly find out the prevous term logs and tell the leaders for asking more args.entries to modified its own uncommitted log
		for reply.NextTryIndex = args.PrevLogIndex - 1; reply.NextTryIndex > 0 && rf.logs[reply.NextTryIndex].Term == term; reply.NextTryIndex-- {
		}

		reply.NextTryIndex++
	} else {
		//only needs to replicate the succeeding logEntries

		//split
		rest := rf.logs[args.PrevLogIndex+1:]
		rf.logs = rf.logs[:args.PrevLogIndex+1]

		if conflicted(rest, args.Entries) || len(args.Entries) > len(rest) {
			//conflicted or follower len lesser than leader's-just overwrite the logs
			/*
				args.entries:	33445
				rest1:			2244	1||1	result:	33445
				rest2:			22445	1||0	result:	33445
				rest3:			3344	0||1	result:	33445
			*/
			rf.logs = append(rf.logs, args.Entries...)
		} else {
			//no conflicted and the length of args.entries is lesser than follower's
			//just let the follower's logs length greater than leader's since it hasn't been commited and will be overwrite after the leader's args.Enties larger than follower's logs
			/*
				args.entries:	33445
				rest1:			334456	0||0	result:	334456
				it's ok that the result is longer than the entries because the commitIndex records the real situation of the rf.logs
			*/
			rf.logs = append(rf.logs, rest...)
		}

		//successfully append entries
		reply.Success = true
		reply.NextTryIndex = args.PrevLogIndex

		// update follower's commitIndex if no conflict
		if args.LeaderCommit > rf.commitIndex {
			if args.LeaderCommit <= rf.getLastLogIndex() {
				rf.commitIndex = args.LeaderCommit
			} else {
				rf.commitIndex = rf.getLastLogIndex()
			}

			go rf.commitLogs()
		}
	}
}

//for server commit its logs
func (rf *Raft) commitLogs() {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	for i := rf.lastApplied + 1; i <= rf.commitIndex; i++ {
		//the commandValid most be true otherwise the applyCh will ignore this applyMsg
		rf.applyCh <- ApplyMsg{CommandValid: true, CommandIndex: i, Command: rf.logs[i].Command}
	}

	rf.lastApplied = rf.commitIndex
}

//detect whether there is a conflict between follower's logs and leader's logs
//flwrLogs-follower's logs	ldrLogs-leader's logs
func conflicted(flwrLogs []LogEntry, ldrLogs []LogEntry) bool {
	for i := range flwrLogs {
		//dont't let the ldrLogs access a possition out of bound
		if i >= len(ldrLogs) {
			break
		}
		if flwrLogs[i].Term != ldrLogs[i].Term {
			return true
		}
	}
	return false
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)

	rf.mu.Lock()
	defer rf.mu.Unlock()

	//if the rf is no longer the leader then return
	if !ok || rf.status != Leader || args.Term != rf.currentTerm {
		return ok
	}

	//if reply return a greater term than rf then the leader turn back to follower
	if reply.Term > rf.currentTerm {
		rf.status = Follower
		rf.votedFor = -1
		rf.currentTerm = reply.Term
		return ok
	}

	if reply.Success {
		//if success it means the follower has the same log entry as the leader
		//match index array update
		rf.matchIndex[server] = args.PrevLogIndex + len(args.Entries)
		//next index array update
		rf.nextIndex[server] = rf.matchIndex[server] + 1
	} else {
		//if false it means it should update the nextIndex by the return nextTryIndex to send the correct log entries in next heartbeat sending
		rf.nextIndex[server] = reply.NextTryIndex
	}

	//now decide whether the log entries can be commit based on the majority
	for N := rf.getLastLogIndex(); N > rf.commitIndex; N-- {
		//conf-solved: the WBZ use the voteCount = 1, while the leader won't update the matchIndex and prevlogIndex of itself, so the >= N should miss the leader voteCount itself
		voteCount := 1
		//the leader only commit the log entries create by its currentTerm
		if rf.logs[N].Term == rf.currentTerm {
			for i := range rf.peers {
				//if the matchIndex has a greater match index then it means log enries is in the follower's
				if rf.matchIndex[i] >= N {
					voteCount++
				}
			}
		}

		if voteCount > len(rf.peers)/2 {
			rf.commitIndex = N
			Trace("update commitIndex and leader->commitLogs()")
			go rf.commitLogs()
			break
		}
	}

	return ok
}

func (rf *Raft) sendAllAppendEntries() {

	rf.mu.Lock()
	defer rf.mu.Unlock()

	//each raft instance should receive different args
	for i := range rf.peers {
		//only when the rf is still the leader, the leader raft send appendEntries request
		if i != rf.me && rf.status == Leader {
			//create the append args
			args := new(AppendEntriesArgs)
			args.Term = rf.currentTerm
			args.LeaderId = rf.me

			//leader commit index
			args.LeaderCommit = rf.commitIndex

			//if the logs is empty:	prevLogIndex == 0
			//with a {0, nil} in it
			args.PrevLogIndex = rf.nextIndex[i] - 1

			//the logs isn't empty so the prevLogTerm can be found in the logs
			if args.PrevLogIndex >= 0 {
				args.PrevLogTerm = rf.logs[args.PrevLogIndex].Term
			}
			//when the nextIndex of follower logs is lesser than leader.nextIndex, it means that the follower's log is incomplete
			//when nextIndex greater than lastLogIndex, it means the follower's logs is up to date and the entries is a empty slice
			if rf.nextIndex[i] <= rf.getLastLogIndex() {
				args.Entries = rf.logs[rf.nextIndex[i]:]
			}

			go rf.sendAppendEntries(i, args, &AppendEntriesReply{})
		}
	}
}

//-----------------------heartbeat rpc sta----------------------
//wrap the sendAppendEntries to heartbeat sending function
func (rf *Raft) sendAllHeartbeat() {
	DPrintf("num-%v sendding heartbeat", rf.me)
	rf.sendAllAppendEntries()
}

//-----------------------vote request rpc sta----------------------

//send votes request to each raft
func (rf *Raft) sendAllRequestVotes() {
	rf.mu.Lock()
	//init request vote args
	args := new(RequestVoteArgs)
	args.Term = rf.currentTerm
	args.CandidateId = rf.me
	args.LastLogIndex = rf.getLastLogIndex()
	args.LastLogTerm = rf.getLastLogTerm()
	rf.mu.Unlock()

	//because the rpc call is a waiting call so use a goroutine to call
	for serverNum := range rf.peers {
		if serverNum != rf.me && rf.status == Candidate {
			go rf.sendRequestVoteAndDetectElectionWin(serverNum, args, new(RequestVoteReply))
		}
	}
}

//A detection function including the sendRequestVote function
//rf-the raft that send vote request
func (rf *Raft) sendRequestVoteAndDetectElectionWin(serverNum int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	//this function will wait until the reply is filled
	ok := rf.sendRequestVote(serverNum, args, reply)

	rf.mu.Lock()
	defer rf.mu.Unlock()
	//return the failed ok
	if !ok {
		return ok
	}

	//the rf become a leader so we don't need the rf.electWin <- below
	//eleciton timeout
	//start a new candidate proccess
	if rf.status != Candidate || args.Term != rf.currentTerm {
		return ok
	}

	//successfully return ok:true
	//but return a greater term
	if reply.Term > rf.currentTerm {
		DPrintf("rf-%v receive a term greater than rf-curTerm reply:%v rf-cuTem:%v", rf.me, reply.Term, rf.currentTerm)

		Warning("Candidate raft-%v:%v receive a bigger term-%v and become follower", rf.me, rf.currentTerm, reply.Term)
		rf.status = Follower
		rf.currentTerm = reply.Term
		rf.votedFor = -1
		return ok
	}
	DPrintf("rf-%v receive a voteRequest reply:%v", rf.me, reply.VoteGranted)
	if reply.VoteGranted {
		rf.voteCount++
		if rf.voteCount > len(rf.peers)/2 {
			Success("Candidate raft-%v:%v become leader with vote:%v", rf.me, rf.currentTerm, rf.voteCount)
			rf.status = Leader
			rf.electWin <- true
		}
	}
	return ok
}

//get last log's term
func (rf *Raft) getLastLogTerm() int {
	return rf.logs[rf.getLastLogIndex()].Term
}

//return the last log index
func (rf *Raft) getLastLogIndex() int {
	return len(rf.logs) - 1
}
