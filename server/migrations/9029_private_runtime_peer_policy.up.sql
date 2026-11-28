-- A server connection can opt into a reciprocal private-runtime grant for two
-- users in one workspace. Read membership from the primary database so losing
-- either membership immediately disables the grant in both directions.
CREATE FUNCTION private_runtime_policy_members(target_workspace uuid) RETURNS uuid[]
LANGUAGE plpgsql STABLE AS $$
DECLARE
    policy text[] := string_to_array(current_setting('multica.private_runtime_peers', true), ',');
    first_user uuid;
    second_user uuid;
BEGIN
    IF array_length(policy, 1) IS DISTINCT FROM 3 OR policy[1] <> target_workspace::text THEN
        RETURN '{}'::uuid[];
    END IF;

    first_user := policy[2]::uuid;
    second_user := policy[3]::uuid;
    IF first_user <> second_user
       AND EXISTS (SELECT 1 FROM member WHERE workspace_id = target_workspace AND user_id = first_user)
       AND EXISTS (SELECT 1 FROM member WHERE workspace_id = target_workspace AND user_id = second_user) THEN
        RETURN ARRAY[first_user, second_user];
    END IF;

    RETURN '{}'::uuid[];
END;
$$;
