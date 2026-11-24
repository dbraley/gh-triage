package fetch

const viewerQuery = `query { viewer { login } }`

// Teams the viewer belongs to, per org. Needs the read:org scope.
const teamsQuery = `
query($login: String!, $after: String) {
  viewer {
    organizations(first: 50, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        login
        teams(first: 100, userLogins: [$login]) { nodes { slug } }
      }
    }
  }
}`

// Lightweight search: only enough to dedupe and apply excludes.
const searchQuery = `
query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        id number url
        repository { nameWithOwner isArchived owner { login } }
      }
    }
  }
}`

const resolveQuery = `
query($url: URI!) {
  resource(url: $url) {
    ... on PullRequest {
      id number url
      repository { nameWithOwner isArchived owner { login } }
    }
  }
}`

// One PR in full. It is a per-PR query (not part of the search) because
// isRequired has to be evaluated against a specific pull request.
// statusCheckRollup hangs off commits(last: 1), so checks from superseded
// commits are never fetched.
const prQuery = `
query($id: ID!) {
  node(id: $id) {
    ... on PullRequest {
      id number title url isDraft createdAt
      repository { nameWithOwner isArchived owner { login } }
      author { __typename login }
      reviewDecision mergeable mergeStateStatus
      isInMergeQueue isMergeQueueEnabled
      mergeQueueEntry { state position estimatedTimeToMerge enqueuedAt }
      autoMergeRequest { enabledAt }
      commits(last: 1) {
        nodes {
          commit {
            oid committedDate
            statusCheckRollup {
              state
              contexts(first: 100) {
                totalCount
                nodes {
                  __typename
                  ... on CheckRun {
                    name status conclusion startedAt completedAt
                    isRequired(pullRequestId: $id)
                  }
                  ... on StatusContext {
                    context state createdAt
                    isRequired(pullRequestId: $id)
                  }
                }
              }
            }
          }
        }
      }
      latestOpinionatedReviews(first: 50) {
        nodes { author { __typename login } state submittedAt commit { oid } }
      }
      latestReviews(first: 50) {
        nodes { author { __typename login } state submittedAt commit { oid } }
      }
      reviewRequests(first: 50) {
        nodes { requestedReviewer { ...reviewer } }
      }
      reviewThreads(first: 100) {
        nodes {
          isResolved isOutdated
          comments(last: 10) { nodes { author { __typename login } createdAt } }
        }
      }
      timelineItems(last: 60, itemTypes: [
        HEAD_REF_FORCE_PUSHED_EVENT, REVIEW_REQUESTED_EVENT, READY_FOR_REVIEW_EVENT,
        ADDED_TO_MERGE_QUEUE_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT
      ]) {
        nodes {
          __typename
          ... on HeadRefForcePushedEvent { createdAt }
          ... on ReviewRequestedEvent { createdAt requestedReviewer { ...reviewer } }
          ... on ReadyForReviewEvent { createdAt }
          ... on AddedToMergeQueueEvent { createdAt }
          ... on RemovedFromMergeQueueEvent { createdAt reason actor { __typename login } }
        }
      }
    }
  }
}
fragment reviewer on RequestedReviewer {
  __typename
  ... on User { login }
  ... on Bot { login }
  ... on Mannequin { login }
  ... on Team { slug organization { login } }
}`
