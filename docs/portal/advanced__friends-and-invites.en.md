# Invites, friends and sharing

This deployment is **invite-only**: the first user to log in automatically
becomes the administrator, and everyone after that has to be invited.

## Inviting a new user (administrator)

The **Invites** page in the navigation bar, which only administrators see, lets
you:

- generate a **one-time invite code** — shown once, at the moment you generate
  it, to pass to your friend;
- or **pre-register their GitHub username** — they sign in to the portal with
  GitHub and are straight in, no code;
- revoke an invite nobody has used yet.

The invited person signs in to the portal with a GitHub account; anyone holding
a code redeems it on the page that greets them.

## Requesting access

There is a path for people who hold no code: sign in with GitHub, land on the
waiting page, and press **Request access**, optionally with one line of at most
200 characters saying who you are.

- one account can have one application waiting at a time; after a decline it
  waits seven days before asking again, and an approved account never sees the
  form again;
- applications are visible only to administrators, and an applicant sees only
  their own;
- administrators find the queue on the **Invites** page, and **Approve** writes
  an invite for that GitHub username — the same road as pre-registering one by
  hand, so the applicant is in the next time they open the portal;
- an administrator with a notification webhook configured gets an
  `access.requested` event.

## Adding a friend

Devices and shares are visible only under your own account by default. Working
across accounts starts with becoming friends:

- on the portal's **Friends** page, look someone up by **exact GitHub
  username** (no fuzzy search, so strangers cannot enumerate users) and send a
  request;
- they accept it on their own **Friends** page, and you become visible to each
  other.

## Sharing a device

Once you are friends, the **Shared devices** page shares one of your devices
with them. A share is not a reduced account: they get what you get on that
device. They drive it with `wanctl exec --target your-namespace/your-device …`,
and every request still goes through that device's own mode, rules and
approvals — the ones you set. Leave the device on per-request approval and you
answer for their commands too; put it in bypass and they are bypassed with you.
Elevated commands are never covered by bypass, for them or for you.

Only two things stay yours alone: unbinding the device, and revoking the share.

**Removing a friend cascades into revoking every share in both directions**,
and they lose access immediately.
