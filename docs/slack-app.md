# Slack app setup

Diderot joins your Slack workspace as an app you create yourself. Once it is
set up, Diderot:

- reads the messages, threads, reactions, and files of the channels its bot
  is in, so Ask can search them;
- answers when someone mentions the bot in a channel, and answers every
  message sent to it directly;
- offers the **Summarize thread** and **Summarize privately** shortcuts on
  messages in a thread.

One Diderot deployment serves one workspace, through one app. Diderot
connects out to Slack, so Slack needs no public URL for it.

You need permission to install apps in the workspace, and a running Diderot
whose admin UI you can sign in to (the [quickstart](quickstart.md), or a
[production](production.md) install).

## 1. Create the app

1. Open <https://api.slack.com/apps> and choose **Create New App**, then
   **From a manifest**.
2. Pick your workspace.
3. Paste the contents of
   [`slack-app-manifest.yaml`](../slack-app-manifest.yaml), replacing the
   example Slack shows, and create the app.

The manifest holds everything the app needs: its bot user, permissions,
events, shortcuts, and Socket Mode, the setting that lets Diderot connect
out to Slack. It also makes the app an agent, so Slack lists it with the
workspace's other agents and people can message it from there. You may
change the app's name and descriptions, and the bot's display name, which is
what people type to mention it (`@diderot` in this guide). Leave the rest as
it is.

The manifest also asks for a user token, which lets Diderot add the bot to
channels from the admin UI ([optional capabilities](#optional-capabilities)).
If you won't use it, delete the `user:` block under `oauth_config.scopes`
before creating the app.

## 2. Install the app and copy its tokens

In the new app's settings:

1. Under **Event Subscriptions**, turn on **Delayed Events**, if your app
   settings offer it, and save. Slack then sends again the events it could
   not deliver while Diderot was restarting, for up to a day, so a question
   asked during a restart still gets an answer.
2. Under **Install App**, install the app to your workspace and allow what
   it asks for.
3. On the same page, copy the **Bot User OAuth Token**. It starts with
   `xoxb-`.
4. If you kept the user token, copy the **User OAuth Token**, starting with
   `xoxp-`. It belongs to you, the person who installed the app: Diderot adds
   and removes the bot as you, and can add it only to channels you can join.
5. Under **Basic Information**, **App-Level Tokens**, choose **Generate Token
   and Scopes**. Name it (for example `diderot`), add the `connections:write`
   scope, generate it, and copy the token. It starts with `xapp-`. The
   manifest can't create this token; it is what lets Diderot connect.

Keep the tokens somewhere safe until the next step. Anyone with the bot token
can read every channel the bot is in.

## 3. Connect Diderot

In Diderot's admin UI, open **Connectors**, then **Slack**
(`/admin/connectors/slack`).

1. Paste the bot token into **Bot token** and the app-level token into **App
   token**. Paste the user token into **User token** if you have one.
2. Choose the **Channel mode**:
   - **Only the channels the bot is in** (the default): Diderot reads the
     channels someone adds the bot to.
   - **Every public channel**: the bot also joins every public channel that
     isn't blocked, and keeps joining new ones. Private channels still need
     the bot added.
3. Under **Operators**, list the emails of the people who look after
   Diderot. Diderot names the first one when it tells someone who to ask for
   help, and sends operators the direct messages you choose:
   - **DM operators about every question**: one message per question asked
     in Slack, over the API, or by an agent, with who asked and a link to the
     conversation. Off by default.
   - **DM operators about agent searches**: one message per search an agent
     runs without asking a question. Off by default; it can be a lot.
   - **DM operators about channel changes**: when the bot is added to a
     private channel, or someone turns Ask on in a channel. On by default.
   - **People whose activity never sends a DM**: emails, such as the
     operators' own, whose questions and searches aren't reported.
4. Turn on **Index documents** if Diderot should read PDF, Word, and Excel
   files posted in the channels you choose ([optional
   capabilities](#optional-capabilities)).
5. Click **Save and turn on**.

Diderot saves the settings, tests them, and turns Slack on if the test
passes. The test takes up to 30 seconds and posts nothing to Slack. It
checks that:

- the bot token works, and has every permission the manifest grants (it
  names any that are missing);
- the app-level token can connect;
- the bot can list channels, and read the latest messages of one it is in;
- each operator is a member of the workspace;
- the user token, if set, works in the same workspace;
- with **Index documents** on, the deployment has somewhere to keep files.

If a check fails, the page lists what to fix, the settings stay saved, and
Slack stays off. Fix the problem and click **Turn on**. **Test** runs the same
checks on what is in the form without saving it.

Once the test passes, **Workspace name** and **Workspace host** are filled
in. Later changes take effect when you click **Save**, without a restart.
**Turn off** stops Diderot reading and answering in Slack; its settings and
everything it has indexed stay.

If a token box is greyed out and asks for `DIDEROT_CONFIG_KEYS`, the
deployment has no key to encrypt pasted tokens yet; see [Connector
settings](configuration.md#connector-settings). A box that says **Set by the
deployment** holds a token your deployment's configuration provides; change
it there.

## 4. Add the bot to channels

Diderot reads a channel only while its bot is in it. To add it, type
`/invite @diderot` in the channel, or mention `@diderot` there with a
question and accept Slack's offer to add it: Diderot answers that question
too. Within five minutes Diderot notices the channel, reads its history,
and keeps up with new messages from then on.

With a user token, you can also add the bot from the admin UI: **Connectors**,
**Slack**, **Channels**, then **Add the bot to a channel**, which takes a
channel's name, ID, or link. Each channel's page there also has:

- **Ask**: whether Diderot answers mentions in the channel. It is on once
  the bot joins, unless someone turned it off.
- **Index documents**: whether Diderot reads the PDF, Word, and Excel files
  posted in the channel. It needs the connector's **Index documents**
  setting.
- **Ask for people outside your organization**: in a channel shared with
  another organization, or with guests in it, Diderot answers no one until
  you allow this, and then answers everyone.
- **Invite the bot** and **Remove the bot**, with a user token.
- **Block this channel**: Diderot stops reading it, even while the bot is in
  it.

What Diderot has read from a channel stays searchable after the bot leaves.
If the bot comes back, or the channel is unblocked, Diderot picks up where it
stopped.

Private channels stay private. Their messages are searchable only by people
who are members of the channel in Slack, matched by email
([Authentication](auth.md#retrieval-scopes)). An answer posted in a channel
draws only on that channel and on what everyone may read.

## 5. Check that it works

1. On the Slack connector page, the last test has passed and Slack is on.
2. In a channel the bot is in, mention it with a question:
   `@diderot what is this channel about?` Slack shows that Diderot is working,
   with a stop button, and the answer arrives in the thread. Pressing stop
   ends the answer without a reply.
3. Open the app in Slack and send it a question in its **Messages** tab, or
   choose one of the suggested prompts at the top, which the manifest sets
   and you may change. It answers there. Slack tells Diderot which channel
   the person has open, so "what happened here this week?" means that
   channel, when it is one they can already search through Diderot. On a paid Slack plan, workspace admins can hide an app's
   agent experience; if Diderot is missing from people's agents, check that
   **AI agent experience** is enabled in the app's settings under **Admin**,
   **Apps and workflows**, **Installed apps**.
4. On a message in a thread with a few replies, open the message's **⋯**
   menu and choose **Summarize thread**. The summary needs a model for
   **Thread summaries** on the **Models** page; without one, the shortcut
   replies that it is not configured.
5. On the connector's **Channels** tab, the channel shows the bot in it and
   a count of messages indexed.

People are matched to their Slack accounts by email, so Slack profile emails
must be the addresses people sign in to Diderot with. Someone whose emails
differ can search only what everyone may read.

## Optional capabilities

| To | You need |
|---|---|
| Add and remove the bot from the admin UI | A user token. The user scopes `channels:write` and `groups:write` let it add the bot to public and private channels. Its owner must be in a private channel to add the bot there. |
| Have the bot join every public channel | **Every public channel** as the channel mode. It uses the `channels:join` permission, which the manifest grants. |
| Index PDF, Word, and Excel files | **Index documents** on the connector and on each channel, file storage for the deployment, and a **Document extraction** model ([Images and documents](configuration.md#images-and-documents)). |
| Make images searchable by what they show | File storage and an **Image captions** model ([Images and documents](configuration.md#images-and-documents)). |
| Summarize threads | A **Thread summaries** model on the **Models** page. |
| Post recurring reports or alert investigations to a channel | The bot in that channel. |

## What each permission is for

The connection test requires every bot permission below except
`channels:join`, which it requires only with **Every public channel**.

| Permission | What Diderot does with it |
|---|---|
| `channels:history`, `groups:history` | Read the messages and threads of public and private channels the bot is in. |
| `im:history`, `mpim:history` | Read direct messages to the bot, and group direct messages it is in. |
| `channels:read`, `groups:read`, `mpim:read` | List channels, see which the bot is in, tell private ones from public, and see who is in them. |
| `channels:join` | Join public channels, with **Every public channel**. |
| `chat:write` | Post answers, summaries, and reports, and show that it is working on an answer. |
| `im:write` | Send direct messages to operators. |
| `assistant:write` | Appear in Slack as an agent. Slack grants it to any app the manifest makes an agent. |
| `app_mentions:read` | Answer a mention in a channel the bot isn't in yet, once Slack adds it there. |
| `commands` | Offer the Summarize shortcuts. |
| `reactions:read`, `reactions:write` | Read reactions, including feedback on answers and 📌 action items, and react to show it has seen a question. |
| `files:read` | Read the images and documents posted in channels. |
| `files:write` | Attach images, such as charts, to answers. |
| `users:read`, `users:read.email` | Show people's names, and match Slack accounts to Diderot sign-ins by email. |
| `usergroups:read` | Show the names of user groups that messages mention. |
| `channels:write`, `groups:write` (user token) | Add the bot to channels and remove it. |

The app also subscribes to messages in every kind of conversation the bot is
in, to mentions of the bot, to reactions, to people joining and leaving
channels, to the stop button, and to the channel someone has open while they
message the bot.

## Changing an existing app

To bring an app made from an older manifest up to date, open its settings,
choose **App Manifest**, replace the manifest with the new file (keep your
own name and descriptions), and save. If Slack asks you to reinstall the app,
do so. The bot token usually stays the same; if Slack shows a new one, paste
it on the connector page. Then click **Test** to confirm the bot has every
permission it needs.

An app made from an older manifest may show a **Tasks** tab, where people can
set the agent recurring work. Diderot doesn't take that work, and the current
manifest turns the tab off, which pauses any tasks people had set up there.

## Keeping it running

- Run one deployment per Slack app. Slack spreads events across every
  connection the app has, so a second Diderot using the same tokens quietly
  takes a share of them.
- Messages sent while Diderot is down are read when it comes back. With
  **Delayed Events** on, questions asked during a short outage are answered
  too.
- If Diderot stops while it is working on an answer (it crashes, or another
  replica takes over), Slack would show it working for up to an hour. When
  Diderot is running again, it replies in that thread that it was
  interrupted and asks the person to ask again, and clears the working
  status, within about a minute.
- If most deliveries fail for about an hour, for example because Diderot is
  stopped or keeps crashing, Slack turns off event delivery for the app and
  emails its owners. Alert on an hour without Slack events, and turn delivery
  back on under **Event Subscriptions** if Slack turned it off.
