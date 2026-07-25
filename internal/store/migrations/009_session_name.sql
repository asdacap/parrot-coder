ALTER TABLE session ADD COLUMN name TEXT NOT NULL DEFAULT '';

-- Older subtasks encoded their name and agent in the display title. Only
-- recover a name when the title exactly matches that format for the selected
-- agent, leaving ordinary user-supplied titles untouched.
UPDATE session
SET name = substr(title, 9, length(title) - length(selected_agent) - 11)
WHERE selected_agent <> ''
  AND length(title) > length(selected_agent) + 11
  AND substr(title, 1, 8) = 'Subtask '
  AND substr(title, -length(selected_agent) - 3) = ' [' || selected_agent || ']';
