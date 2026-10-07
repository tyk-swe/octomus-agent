package git

const maintenanceMergeStatusQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    nameWithOwner squashMergeAllowed
    pullRequest(number:$number){
      number url state
      headRefName headRefOid headRepository{nameWithOwner}
      baseRefName baseRefOid repository{nameWithOwner}
      isDraft isMergeQueueEnabled
      reviewDecision mergeable mergeStateStatus
      additions deletions changedFiles
      mergedAt mergeCommit{oid}
      commits(last:1){
        nodes{commit{oid statusCheckRollup{state contexts{totalCount}}}}
      }
    }
  }
}`
