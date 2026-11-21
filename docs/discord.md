# Discord

The Discord source indexes the messages of the Discord channels you choose,
in every server its bot is in: text and announcement channels with their
public threads, and the posts of forum and media channels. Ask and the MCP
tools then search them like any other source (`sources: ["discord"]`), read a
channel with `list`, and expand a message to its thread or the conversation
around it.

Diderot can also answer in Discord, as it does in Slack: see
[Ask in Discord](#ask-in-discord). It is off until you turn it on.

## What gets indexed

- One result per message: its text with mentions written as names, the
  message it replies to, forwarded text, embed titles, attachment names, and
  reaction counts. Joins, pins and other notices are left out.
- Thread messages are grouped with the message that started the thread (or
  the forum post's first message), so `expand` returns the whole thread.
- Images, attached or embedded, are captioned when the deployment has file
  storage (`DIDEROT_IMAGES_BUCKET`) and a captioning model. PDF, Word and
  Excel files are extracted to text when **Index documents** is on.
- Edits replace a message; deletions remove it. Live changes arrive over
  Discord's gateway. A check every few minutes picks up messages posted while
  the connection was down, and a daily re-read of the last seven days
  catches edits and deletions it missed.
- Private threads are not indexed, nor are the bot's own messages (its
  answers).

## Who can read it

Each channel's messages carry the channel's own scope, `discord-channel:<id>`.
Discord decides who sees a channel with roles and permission overwrites;
Diderot does not know which Discord account belongs to which of its users,
so it doesn't copy those permissions. Instead, you choose who in Diderot is
granted the channel's scope when you choose the channel:

- **Everyone who can use Diderot** (the default for a channel everyone in the
  server can see).
- **Only the groups the authorization policy grants a scope**: name a scope,
  such as `discord-mods`, and grant `retrieve` on `corpus:discord-mods` to
  the groups that should read it in the
  [authorization policy](auth.md#authorization-policy).

A channel that `@everyone` cannot view in Discord has no default: Diderot
asks you to confirm who may read it before it indexes it. Changing a
channel's visibility applies at once, to every question asked after it.

A Discord author appears as their Discord display name. Discord never tells a
bot anyone's email, so a Discord author is not matched with the same person
in Slack or other sources.

## Set up the bot

1. In the [Discord Developer Portal](https://discord.com/developers/applications),
   create an application, for example "Diderot".
2. On its **Bot** page:
   - Turn on **Message Content Intent** under **Privileged Gateway
     Intents**. Without it Discord sends a bot empty messages. The Server
     Members and Presence intents are not needed.
   - Turn off **Public Bot**, so only you can add it to servers.
   - Click **Reset Token** and copy the token. Treat it like a password.
3. Invite the bot to each server you want indexed with this link, putting
   your application's ID (on its **General Information** page) in place of
   `APPLICATION_ID`:

   ```text
   https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot&permissions=66560
   ```

   `66560` is **View Channels** and **Read Message History**, all the bot
   needs to index. Pick the server and authorize; you need the **Manage
   Server** permission there. To answer in Discord too, use the invite link
   the connector's page shows once it is on: it adds the permissions Ask
   needs and the `applications.commands` scope
   ([Ask in Discord](#set-up-ask)).
4. For a channel `@everyone` can't see, give the bot's role (or the bot)
   **View Channel** and **Read Message History** in the channel's
   permissions, or Diderot can't read it.

## Connect Diderot

1. Open **Connectors → Discord** in the admin pages (`/admin/connectors/discord`).
2. Paste the bot token into **Bot token**. Leave **Read history from the
   last** at 0 to index each channel's whole history, or give a number of days
   to start more recently. Turn on **Index documents** if you want attached
   documents extracted (it needs file storage).
3. Click **Save and turn on**. The test signs in as the bot, checks the
   Message Content intent, and counts its servers; it says what to fix if
   anything fails.
4. Within a minute the **Channels** section lists every server and every
   channel the bot can see. Open **Index** on each channel you want, choose
   who may read it, and confirm. Diderot reads its history, then keeps it
   up to date.

## Check it works

- The channel's row shows **reading**, then **yes** with a message count.
- **Live updates** in the Status section says **connected**.
- Ask a question whose answer is in the channel, or call the MCP `list` tool
  with `channels: ["#your-channel"]`; each result links to its message in
  Discord.

## Change or remove a channel

Open **Change** on a chosen channel to change who may read it, to read its
history again, or to remove it. Removing a channel takes its messages out of
search. **Read everything again** at the bottom of the page re-reads every
chosen channel.

## Ask in Discord

Diderot answers in Discord the way it answers in Slack, through the same
engine (`internal/askchat`):

- **Mention** the bot in a channel where Ask is on. It reacts 👀, starts a
  thread from your question, and answers there; ✅ replaces 👀 once the
  answer is in. Mention it again in the thread to follow up. The thread is
  the conversation, and what was said in it since the last answer is passed
  along (only where everyone who can see the channel can read its history).
  When you mention it in a thread or forum post, it answers in place.
- **Direct message** the bot, when **Answer direct messages** is on. The
  DM is one conversation.
- **`/ask question:…`** in a channel posts the question and answers in a
  thread, like a mention. Add **`private:True`** to get an answer only you
  see, built from what you can read; in the bot's DMs `/ask` is always
  private. A private answer comes back through Discord's reply to the
  command, which Discord accepts for 15 minutes, so a private question has
  13 minutes to finish.
- **Summarize thread** and **Summarize thread privately**, in a message's
  **Apps** menu, summarize the thread the message is in (or the one it
  started) with the thread-summary model, for the channel or only for you.
  Pinned messages, and messages with a 📌 reaction, become action items.

While it works the thread shows **Working on it…** with a **Stop** button
and the bot typing. **Stop** ends the turn; nothing more is posted and the
placeholder says **Stopped.** If Diderot restarts mid-answer, the leader
replica takes the placeholder down and says the answer was interrupted.

Answers are split into messages under Discord's 2,000-character limit,
never ping anyone (`@everyone` included), and don't unfurl links. Grafana
panels an answer chose are attached as PNG files. React 👍 or 👎 to an
answer to rate it; ratings appear with Slack's on the feedback page.

### Who an answer may read

Discord gives a bot no email, so a Discord user has no Diderot identity.
An answer therefore reads only what its audience can read in Discord.
Diderot works that out from the server's roles and the channels' permission
overwrites, which it reads from Discord at each question (a failed read
grants nothing):

- **An answer in a channel or thread** is read by everyone who can see the
  channel. It uses public content (what anyone on the internet could read),
  the server's chosen channels that every member can read (`@everyone` may
  view them and read their history, and no overwrite denies either), and
  the channel itself when it is chosen and everyone who can view it can
  read its history. It never uses more because of who asked, and never a
  policy scope, so a private channel's contents never reach a public
  channel. In a [trusted server](#trusted-servers) it also uses `internal`.
- **A DM, a private `/ask`, or a private summary** is read by its asker
  only. It uses public content and the chosen channels, in the servers
  where Ask is on, that the asker can view and read the history of. It
  also uses `internal` when the asker is a member of a trusted server
  where Ask is on. Only members of a server where Ask is on get answers.
- A conversation continues only while its audience's access is unchanged:
  when a channel opens or closes, or the asker's roles change, the next
  question starts a new conversation, so no one can ask the model to repeat
  what it read under access they no longer have.

Usernames are only shown as who asked: a Discord account is never matched
to a Diderot user, so no answer uses one person's own access (their agent
sessions, their private Slack channels).

### Trusted servers

A server whose members are all your organisation's own people can be
**trusted** on the connector's page (**Trust *server***, off by default).
Ask in a trusted server may then also answer from what everyone who can
use Diderot may read (the `internal` scope: Notion, Linear, Markdown
folders, agent sessions that are not owner-only, and the Discord channels
whose visibility is everyone who can use Diderot). Anyone who is a member
of the server can then ask about that content, so trust a server only if
membership is limited to your people.

- An answer in a trusted server's channel or thread uses `internal` on top
  of what its audience can read, because only members see it. It does not
  do so in an announcement channel (its posts can be published to the
  servers that follow it), or while the server can be read without joining
  (it is in the server directory, or previews are on). Answers there stay
  as in an untrusted server.
- A DM, a private `/ask` or a private summary uses `internal` when its
  asker is a member of a trusted server where Ask is on, read at each
  question. A guest (invited to one voice channel) or a member still in
  Membership Screening doesn't count, and a membership in an untrusted
  server never does.
- Policy scopes (`corpus:<name>`) are never used.
- Trusting a server, or ending that trust, starts new conversations: a
  thread or DM that continues afterwards does so with the new access, in a
  new session.
- Bots and bridges in a trusted server that relay its messages elsewhere
  take its answers with them; remove them, or don't trust the server.

### Set up Ask

1. Add the permissions Ask needs: re-invite the bot with the **invite link**
   on the connector's page (or give its role, in each server or channel,
   **View Channel**, **Read Message History**, **Send Messages**, **Send
   Messages in Threads**, **Create Public Threads**, **Add Reactions**,
   **Embed Links**, **Attach Files** and **Use Application Commands**). The
   link includes the `applications.commands` scope, which `/ask` and the
   summary commands need.
2. On the connector's page, under **Ask in Discord**, turn Ask on for each
   server, and off for any channel where it should not answer (a channel's
   threads follow it). Turn on **Answer direct messages** to answer DMs.
3. The commands are registered when the connector starts; Discord may take a
   few minutes to show them the first time.

Ask needs Ask itself to be on in Diderot (`DIDEROT_ASK_ENABLED`) and, for
summaries, a model chosen for thread summaries on the **Models** page.

## When something is wrong

- **Live updates stopped: the Message Content intent is not switched on**:
  turn it on on the Bot page, then turn the connector off and on.
- **Live updates stopped: Discord refused the bot token**: the token was
  reset; paste the new one.
- **can't be read** on a channel: the bot lost **View Channel** or **Read
  Message History** there. Fix its permissions; the next read picks it up.
- A channel missing from the list: the bot can't see it, or it is a voice or
  stage channel, which Diderot doesn't index.
- A channel listed under **No longer in Discord**: it was deleted, or the bot
  can no longer see it. Remove it to take its messages out of search.
